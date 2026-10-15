// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// maxCapture bounds what is kept of any one body for -raw. The platform client still reads
	// the whole response under its own limits; only the copy is capped.
	maxCapture = 256 << 10
	// clientTimeout is a backstop: each platform client applies its own per-request deadline,
	// and the run as a whole is bounded by -timeout.
	clientTimeout = 2 * time.Minute
)

// redditReportPathRe is Reddit's report query, the one non-token POST a Reddit metrics read makes.
var redditReportPathRe = regexp.MustCompile(`(?i)/ad_accounts/[^/]+/reports$`)

// isTokenEndpoint reports whether path is an OAuth token exchange: Reddit's /api/v1/access_token,
// Microsoft's /oauth2/v2.0/token, LinkedIn's /oauth/v2/accessToken.
func isTokenEndpoint(path string) bool {
	p := strings.ToLower(strings.TrimRight(path, "/"))
	return strings.HasSuffix(p, "/access_token") || strings.HasSuffix(p, "/token") || strings.HasSuffix(p, "/accesstoken")
}

// allowedRequest is the read-only policy every upstream request must pass before it is sent.
// GET and HEAD are reads. A POST is admitted only where a metrics read is made of POSTs: a token
// exchange, Reddit's report query, and Microsoft Reporting's submit and poll (which build a
// report file; they change nothing in the account). Every other method and path — any create,
// update, PATCH, PUT or DELETE — is refused without contacting the platform.
func allowedRequest(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
		p := strings.ToLower(strings.TrimRight(path, "/"))
		return isTokenEndpoint(p) ||
			redditReportPathRe.MatchString(p) ||
			strings.HasSuffix(p, "/generatereport/submit") ||
			strings.HasSuffix(p, "/generatereport/poll")
	}
	return false
}

// exchange is one upstream request and what came back, as captured for -raw.
type exchange struct {
	method, url string
	token       bool // an OAuth token exchange: bodies withheld entirely
	reqBody     []byte
	status      int
	refused     bool
	err         string
	resp        *cappedBuffer
}

// recorder is the HTTP layer every platform client in a run is built on: it enforces
// allowedRequest, harvests credential-bearing headers and token-exchange results into the secret
// set, and keeps a capped copy of every exchange for -raw.
type recorder struct {
	base    http.RoundTripper
	secrets *secretSet

	mu        sync.Mutex
	exchanges []*exchange
}

func newRecorder(secrets *secretSet) *recorder {
	return &recorder{base: http.DefaultTransport, secrets: secrets}
}

// client returns the *http.Client handed to the platform clients. They force their own
// no-redirect policy on whatever client they are given.
func (r *recorder) client() *http.Client {
	return &http.Client{Transport: r, Timeout: clientTimeout}
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	ex := &exchange{method: req.Method, url: displayURL(req.URL), token: isTokenEndpoint(req.URL.Path)}
	r.mu.Lock()
	r.exchanges = append(r.exchanges, ex)
	r.mu.Unlock()

	for name, vals := range req.Header {
		if secretKeyRe.MatchString(name) {
			for _, v := range vals {
				r.secrets.addHeaderValue(v)
			}
		}
	}
	if !allowedRequest(req.Method, req.URL.Path) {
		ex.refused = true
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: refused to send %s %s", errReadOnly, req.Method, ex.url)
	}
	if !ex.token && req.GetBody != nil {
		// A copy, so the request itself is not consumed. Token exchange bodies are never
		// copied: they are the client secret and refresh token.
		if rc, err := req.GetBody(); err == nil {
			ex.reqBody, _ = io.ReadAll(io.LimitReader(rc, maxCapture))
			_ = rc.Close()
		}
	}

	resp, err := r.base.RoundTrip(req)
	if err != nil {
		ex.err = err.Error()
		return nil, err
	}
	ex.status = resp.StatusCode
	if ex.token {
		// Record the issued tokens so they are scrubbed wherever they reappear. The body is
		// read whole (a token response is small) and handed back unchanged.
		// cap+1, so an oversized body is refused rather than handed back cut short as if
		// it were the whole response (io.LimitReader reports EOF, not an error, at the cap).
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxCapture+1))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		if len(body) > maxCapture {
			return nil, fmt.Errorf("token exchange response exceeds %d bytes", maxCapture)
		}
		var doc any
		if json.Unmarshal(body, &doc) == nil {
			r.secrets.addIssuedTokens(doc)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	ex.resp = &cappedBuffer{}
	resp.Body = &teeBody{ReadCloser: resp.Body, copy: ex.resp}
	return resp, nil
}

// print writes every captured exchange, redacted. It runs after the read, success or failure:
// a failing read's raw response is usually the most useful thing to see.
func (r *recorder) print(out *scrubWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out.printf("raw upstream exchanges: %d\n", len(r.exchanges))
	for i, ex := range r.exchanges {
		out.printf("[%d] %s %s", i+1, ex.method, ex.url)
		switch {
		case ex.refused:
			out.printf(" -> REFUSED (not a read)\n")
			continue
		case ex.err != "":
			out.printf(" -> transport error: %s\n", ex.err)
			continue
		}
		out.printf(" -> %d\n", ex.status)
		if ex.token {
			out.printf("    (OAuth token exchange: request and response bodies withheld)\n")
			continue
		}
		if len(ex.reqBody) > 0 {
			out.printf("    request body:\n%s\n", indent(redactBody(ex.reqBody)))
		}
		if ex.resp != nil {
			out.printf("    response body:\n%s\n", indent(renderResponse(ex.resp)))
		}
	}
}

// renderResponse decodes a zip (Microsoft's report download) into its files before redaction.
func renderResponse(b *cappedBuffer) string {
	body := b.buf.Bytes()
	note := ""
	if b.truncated {
		note = fmt.Sprintf("\n(truncated: only the first %d bytes were kept)", maxCapture)
	}
	if bytes.HasPrefix(body, []byte("PK\x03\x04")) && !b.truncated {
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err == nil {
			var sb strings.Builder
			for _, f := range zr.File {
				fmt.Fprintf(&sb, "zip entry %s:\n", f.Name)
				rc, oerr := f.Open()
				if oerr != nil {
					fmt.Fprintf(&sb, "(unreadable: %v)\n", oerr)
					continue
				}
				data, _ := io.ReadAll(io.LimitReader(rc, maxCapture))
				_ = rc.Close()
				sb.WriteString(redactBody(data))
				sb.WriteString("\n")
			}
			return sb.String()
		}
	}
	return redactBody(body) + note
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n")
}

// cappedBuffer keeps at most maxCapture bytes and remembers whether it dropped any.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) write(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := maxCapture - c.buf.Len()
	if room <= 0 {
		c.truncated = c.truncated || len(p) > 0
		return
	}
	if len(p) > room {
		p = p[:room]
		c.truncated = true
	}
	c.buf.Write(p)
}

// teeBody copies what the platform client reads into the capture buffer.
type teeBody struct {
	io.ReadCloser
	copy *cappedBuffer
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.copy.write(p[:n])
	}
	return n, err
}
