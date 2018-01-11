package httpupstream

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"
	"time"

	"github.com/opentracing-contrib/go-stdlib/nethttp"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

// Auth is the httpupstream authenticater
type Auth struct {
	upstream   *url.URL
	skipverify bool
	timeout    time.Duration
}

// NewAuth creates an httpupstream authenticater
func NewAuth(upstream *url.URL, timeout time.Duration, skipverify bool) (*Auth, error) {
	a := &Auth{
		upstream:   upstream,
		skipverify: skipverify,
		timeout:    timeout,
	}

	return a, nil
}

// Authenticate the user
func (a *Auth) Authenticate(username, password string) (bool, error) {
	c := &http.Client{
		Timeout: a.timeout,
	}

	if a.upstream.Scheme == "https" && a.skipverify {
		c.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	req, err := http.NewRequest("GET", a.upstream.String(), nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(username, password)

	resp, err := c.Do(req)
	if err != nil {
		return false, err
	}

	if resp.StatusCode != 200 {
		return false, nil
	}

	return true, nil
}

//AuthenticateWithContext traced authentication
func (a *Auth) AuthenticateWithContext(ctx context.Context, username, password string) (bool, error) {
	parentSpan := opentracing.SpanFromContext(ctx)
	tracer := parentSpan.Tracer()
	span := tracer.StartSpan("HTTP Upstream", opentracing.ChildOf(parentSpan.Context()))
	ext.SpanKind.Set(span, "client")
	ext.Component.Set(span, "net/http")
	span.SetTag("http.method", "GET")
	span.SetTag("http.url", a.upstream.String())
	defer span.Finish()
	req, _ := http.NewRequest("GET", a.upstream.String(), nil)
	req.SetBasicAuth(username, password)
	tracer.Inject(span.Context(), opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(req.Header))

	client := &http.Client{Transport: &nethttp.Transport{}}
	if a.upstream.Scheme == "https" && a.skipverify {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	rsp, err := client.Do(req)
	span.SetTag("http.status_code", rsp.StatusCode)
	if err != nil {
		span.SetTag("error", true)
		return false, err
	}

	if rsp.StatusCode != 200 {
		span.SetTag("error", true)
		return false, nil
	}

	return true, nil
}
