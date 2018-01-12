package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/opentracing/opentracing-go"

	"github.com/dgrijalva/jwt-go"
	"github.com/tarent/loginsrv/logging"
	"github.com/tarent/loginsrv/model"
	"github.com/tarent/loginsrv/oauth2"
)

const contentTypeHTML = "text/html; charset=utf-8"
const contentTypeJWT = "application/jwt"
const contentTypePlain = "text/plain"

// Handler is the mail login handler.
// It serves the login ressource and does the authentication against the backends or oauth provider.
type Handler struct {
	backends []Backend
	oauth    oauthManager
	config   *Config
}

// NewHandler creates a login handler based on the supplied configuration.
func NewHandler(config *Config) (*Handler, error) {
	if len(config.Backends) == 0 && len(config.Oauth) == 0 {
		return nil, errors.New("No login backends or oauth provider configured")
	}

	backends := []Backend{}
	for pName, opts := range config.Backends {
		p, exist := GetProvider(pName)
		if !exist {
			return nil, fmt.Errorf("No such provider: %v", pName)
		}
		b, err := p(opts)
		if err != nil {
			return nil, err
		}
		backends = append(backends, b)
	}

	oauth := oauth2.NewManager()
	for providerName, opts := range config.Oauth {
		err := oauth.AddConfig(providerName, opts)
		if err != nil {
			return nil, err
		}
	}

	return &Handler{
		backends: backends,
		config:   config,
		oauth:    oauth,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, h.config.LoginPath) {
		h.respondNotFound(w, r)
		return
	}

	_, err := h.oauth.GetConfigFromRequest(r)
	if err == nil {
		h.handleOauth(w, r)
		return
	}

	h.handleLogin(w, r)
	return
}

func (h *Handler) handleOauth(w http.ResponseWriter, r *http.Request) {
	startedFlow, authenticated, userInfo, err := h.oauth.Handle(w, r)

	if startedFlow {
		// the oauth flow started
		return
	}

	if err != nil {
		logging.Application(r.Header).WithError(err).Error()
		h.respondError(w, r)
		return
	}

	if authenticated {
		logging.Application(r.Header).
			WithField("username", userInfo.Sub).Info("successfully authenticated")
		h.respondAuthenticated(w, r, userInfo)
		return
	}
	logging.Application(r.Header).
		WithField("username", userInfo.Sub).Info("failed authentication")

	h.respondAuthFailure(w, r)
	return
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if !(r.Method == "GET" || r.Method == "DELETE" ||
		(r.Method == "POST" &&
			(strings.HasPrefix(contentType, "application/json") ||
				strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
				strings.HasPrefix(contentType, "multipart/form-data") ||
				contentType == ""))) {
		h.respondBadRequest(w, r)
		return
	}

	r.ParseForm()
	if r.Method == "DELETE" || r.FormValue("logout") == "true" {
		h.deleteToken(w)
		if h.config.LogoutURL != "" {
			w.Header().Set("Location", h.config.LogoutURL)
			w.WriteHeader(303)
			return
		}
		writeLoginForm(w,
			loginFormData{
				Config: h.config,
			})
		return
	}

	if r.Method == "GET" {
		userInfo, valid := h.GetToken(r, "")
		writeLoginForm(w,
			loginFormData{
				Config:        h.config,
				Authenticated: valid,
				UserInfo:      userInfo,
			})
		return
	}

	if r.Method == "POST" {
		username, password, rtoken, err := getCredentials(r)

		if err != nil {
			h.respondBadRequest(w, r)
			return
		}

		if username != "" {
			// No token found or credentials found, assuming new authentication
			h.handleAuthentication(w, r, username, password)
			return
		}
		userInfo, valid := h.GetToken(r, rtoken)
		if valid {
			h.handleRefresh(w, r, userInfo)
			return
		}
		h.respondBadRequest(w, r)
		return
	}
}

func (h *Handler) handleAuthentication(w http.ResponseWriter, r *http.Request, username string, password string) {

	tracer := opentracing.GlobalTracer()
	var authenticated bool
	var userInfo model.UserInfo
	var err error
	if tracer == nil {
		authenticated, userInfo, err = h.authenticate(username, password)
	} else {
		authenticated, userInfo, err = h.authenticateWithContext(r.Context(), username, password)
	}

	if err != nil {
		logging.Application(r.Header).WithError(err).Error()
		h.respondError(w, r)
		return
	}

	if authenticated {
		logging.Application(r.Header).
			WithField("username", username).Info("successfully authenticated")
		h.respondAuthenticated(w, r, userInfo)
		return
	}
	logging.Application(r.Header).
		WithField("username", username).Info("failed authentication")

	h.respondAuthFailure(w, r)
}

func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request, userInfo model.UserInfo) {
	if userInfo.Refreshes >= h.config.JwtRefreshes {
		h.respondMaxRefreshesReached(w, r)
	} else {
		userInfo.Refreshes++
		h.respondAuthenticated(w, r, userInfo)
		logging.Application(r.Header).WithField("username", userInfo.Sub).Info("refreshed jwt")
	}
}

func (h *Handler) deleteToken(w http.ResponseWriter) {
	cookie := &http.Cookie{
		Name:     h.config.CookieName,
		Value:    "delete",
		HttpOnly: true,
		Expires:  time.Unix(0, 0),
		Path:     "/",
	}
	if h.config.CookieDomain != "" {
		cookie.Domain = h.config.CookieDomain
	}
	http.SetCookie(w, cookie)
}

func (h *Handler) respondAuthenticated(w http.ResponseWriter, r *http.Request, userInfo model.UserInfo) {
	userInfo.Expiry = time.Now().Add(h.config.JwtExpiry).Unix()
	token, err := h.createToken(userInfo)
	if err != nil {
		logging.Application(r.Header).WithError(err).Error()
		h.respondError(w, r)
		return
	}

	if wantHTML(r) {
		cookie := &http.Cookie{
			Name:     h.config.CookieName,
			Value:    token,
			HttpOnly: h.config.CookieHTTPOnly,
			Path:     "/",
		}
		if h.config.CookieExpiry != 0 {
			cookie.Expires = time.Now().Add(h.config.CookieExpiry)
		}
		if h.config.CookieDomain != "" {
			cookie.Domain = h.config.CookieDomain
		}

		http.SetCookie(w, cookie)

		w.Header().Set("Location", h.config.SuccessURL)
		w.WriteHeader(303)
		return
	}

	w.Header().Set("Content-Type", contentTypeJWT)
	w.WriteHeader(200)
	fmt.Fprintf(w, "%s", token)
}

func (h *Handler) createToken(userInfo jwt.Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS512, userInfo)
	return token.SignedString([]byte(h.config.JwtSecret))
}

func (h *Handler) GetToken(r *http.Request, rtoken string) (userInfo model.UserInfo, valid bool) {
	if rtoken == "" {
		c, err := r.Cookie(h.config.CookieName)
		if err != nil {
			return model.UserInfo{}, false
		}
		rtoken = c.Value
	}

	token, err := jwt.ParseWithClaims(rtoken, &model.UserInfo{}, func(*jwt.Token) (interface{}, error) {
		return []byte(h.config.JwtSecret), nil
	})
	if err != nil {
		return model.UserInfo{}, false
	}

	u, ok := token.Claims.(*model.UserInfo)
	if !ok {
		return model.UserInfo{}, false
	}

	return *u, u.Valid() == nil
}

func (h *Handler) respondError(w http.ResponseWriter, r *http.Request) {
	if wantHTML(r) {
		username, _, _, _ := getCredentials(r)
		writeLoginForm(w,
			loginFormData{
				Error:    true,
				Config:   h.config,
				UserInfo: model.UserInfo{Sub: username},
			})
		return
	}
	w.Header().Set("Content-Type", contentTypePlain)
	w.WriteHeader(500)
	fmt.Fprintf(w, "Internal Server Error")
}

func (h *Handler) respondBadRequest(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(400)
	fmt.Fprintf(w, "Bad Request: Method or content-type not supported")
}

func (h *Handler) respondNotFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(404)
	fmt.Fprintf(w, "Not Found: The requested page does not exist")
}

func (h *Handler) respondMaxRefreshesReached(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(403)
	fmt.Fprint(w, "Max JWT refreshes reached")
}

func (h *Handler) respondAuthFailure(w http.ResponseWriter, r *http.Request) {
	if wantHTML(r) {
		w.Header().Set("Content-Type", contentTypeHTML)
		w.WriteHeader(403)
		username, _, _, _ := getCredentials(r)
		writeLoginForm(w,
			loginFormData{
				Failure:  true,
				Config:   h.config,
				UserInfo: model.UserInfo{Sub: username},
			})
		return
	}

	w.Header().Set("Content-Type", contentTypePlain)
	w.WriteHeader(403)
	fmt.Fprintf(w, "Wrong credentials")
}

func wantHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func getCredentials(r *http.Request) (string, string, string, error) {
	if r.Header.Get("Content-Type") == "application/json" {
		m := map[string]string{}
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return "", "", "", err
		}
		err = json.Unmarshal(body, &m)
		if err != nil {
			return "", "", "", err
		}
		return m["username"], m["password"], m["token"], nil
	}
	return r.PostForm.Get("username"), r.PostForm.Get("password"), r.PostForm.Get("token"), nil
}

func (h *Handler) authenticate(username, password string) (bool, model.UserInfo, error) {
	for _, b := range h.backends {
		authenticated, userInfo, err := b.Authenticate(username, password)
		if err != nil {
			return false, model.UserInfo{}, err
		}
		if authenticated {
			return authenticated, userInfo, nil
		}
	}
	return false, model.UserInfo{}, nil
}

func (h *Handler) authenticateWithContext(ctx context.Context, username, password string) (bool, model.UserInfo, error) {
	for _, b := range h.backends {
		authenticated, userInfo, err := b.AuthenticateWithContext(ctx, username, password)
		if err != nil {
			return false, model.UserInfo{}, err
		}
		if authenticated {
			return authenticated, userInfo, nil
		}
	}
	return false, model.UserInfo{}, nil
}

type oauthManager interface {
	Handle(w http.ResponseWriter, r *http.Request) (
		startedFlow bool,
		authenticated bool,
		userInfo model.UserInfo,
		err error)
	AddConfig(providerName string, opts map[string]string) error
	GetConfigFromRequest(r *http.Request) (oauth2.Config, error)
}
