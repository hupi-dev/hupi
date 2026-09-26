package provider

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// Found running the real benchmark harness (cmd/hupi-bench) against a
// real OpenAI account at scale: a 429 (rate limit) or a transient 5xx
// from the upstream provider had no retry at all — one rate-limited
// request failed the entire chat-completion call outright, which then
// propagates as a hard error all the way up through gateway.Handler to
// the caller. A production deployment hitting its own account's rate
// limit under real traffic would see the exact same thing: real,
// legitimate requests failing outright instead of a self-throttling
// backoff, which is what every serious provider's own client library
// does by default. This is a real product gap, not just a benchmark
// harness one.
const (
	maxRetries     = 5
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
)

// retryableStatus reports whether an HTTP status from a provider is
// worth retrying — 429 (rate limit) and 5xx (transient upstream
// failures). Never a 4xx client error like 400/401/404, which will just
// fail identically on every retry.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// retryDelay computes the backoff before the next attempt (1-indexed).
// OpenAI's own 429 responses always carry a Retry-After header (in
// seconds) telling the caller exactly how long to wait — honoring that
// real hint beats guessing with our own backoff schedule. Falls back to
// exponential backoff with jitter when no such header is present (a 5xx,
// or a vendor that doesn't send one). Jitter matters even for a single
// caller like this harness: without it, a burst of requests that all hit
// the same 429 at once would retry in lockstep and immediately re-trip
// the same limit.
func retryDelay(attempt int, retryAfter string) time.Duration {
	if retryAfter != "" {
		if secs, err := strconv.ParseFloat(retryAfter, 64); err == nil && secs > 0 {
			d := time.Duration(secs * float64(time.Second))
			if d > retryMaxDelay {
				return retryMaxDelay
			}
			return d
		}
	}
	d := retryBaseDelay * time.Duration(uint(1)<<uint(attempt-1))
	if d > retryMaxDelay {
		d = retryMaxDelay
	}
	jitter := time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	return jitter
}

// sendWithRetry sends an HTTP request built fresh by buildReq on every
// attempt — a request body is a single-read io.Reader, so a genuine retry
// needs a brand new *http.Request, not the same one resent — retrying on
// a 429/5xx status or a network-level error from client.Do, up to
// maxRetries attempts total. Returns the final status code and the fully
// read response body (already closed) once a non-retryable outcome is
// reached.
func sendWithRetry(ctx context.Context, client *http.Client, name string, buildReq func() (*http.Request, error)) (status int, respBody []byte, err error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, buildErr := buildReq()
		if buildErr != nil {
			return 0, nil, buildErr
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			if attempt == maxRetries {
				return 0, nil, fmt.Errorf("provider %s: request failed: %w", name, doErr)
			}
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(retryDelay(attempt, "")):
			}
			continue
		}
		b, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return resp.StatusCode, nil, fmt.Errorf("provider %s: read response body: %w", name, readErr)
		}
		if retryableStatus(resp.StatusCode) && attempt < maxRetries {
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(retryDelay(attempt, resp.Header.Get("Retry-After"))):
			}
			continue
		}
		return resp.StatusCode, b, nil
	}
	// Unreachable: the loop always returns by the maxRetries-th iteration.
	return 0, nil, fmt.Errorf("provider %s: retry loop exhausted unexpectedly", name)
}

// connectWithRetry is sendWithRetry's counterpart for a streamed
// response: a 429/5xx arrives immediately, before any SSE bytes flow, so
// that initial connect phase can retry exactly like a non-streamed
// request — but a *successful* response's body must stay open and
// unread for the caller to stream from, unlike sendWithRetry, which
// always reads the body fully into memory. Once streaming has actually
// started there's no retrying a partial generation; only this initial
// connect is covered.
func connectWithRetry(ctx context.Context, client *http.Client, name string, buildReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, buildErr := buildReq()
		if buildErr != nil {
			return nil, buildErr
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			if attempt == maxRetries {
				return nil, fmt.Errorf("provider %s: request failed: %w", name, doErr)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay(attempt, "")):
			}
			continue
		}
		if retryableStatus(resp.StatusCode) && attempt < maxRetries {
			retryAfter := resp.Header.Get("Retry-After")
			resp.Body.Close()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryDelay(attempt, retryAfter)):
			}
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("provider %s: retry loop exhausted unexpectedly", name)
}
