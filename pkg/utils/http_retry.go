package utils

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const maxRetries = 3

var retryDelayUnit = time.Second

// ShouldRetry reports whether an HTTP status code represents a transient error
// worth retrying (429 Too Many Requests or any 5xx).
func ShouldRetry(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode >= 500
}

func DoRequestWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := range maxRetries {
		if i > 0 && resp != nil {
			resp.Body.Close()
		}

		resp, err = client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				break
			}
			if !ShouldRetry(resp.StatusCode) {
				break
			}
		}

		if i < maxRetries-1 {
			if err = SleepWithCtx(req.Context(), RetryDelayForAttempt(resp, i)); err != nil {
				if resp != nil {
					resp.Body.Close()
				}
				return nil, fmt.Errorf("failed to sleep: %w", err)
			}
		}
	}
	return resp, err
}

// RetryDelayForAttempt returns the backoff delay before the given retry attempt
// (0-indexed). For 429 responses it honors a Retry-After header (seconds or HTTP
// date); otherwise it uses a linear backoff of retryDelayUnit*(attempt+1).
func RetryDelayForAttempt(resp *http.Response, attempt int) time.Duration {
	fallback := retryDelayUnit * time.Duration(attempt+1)
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return fallback
	}

	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter == "" {
		return fallback
	}

	if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}

	if when, err := http.ParseTime(retryAfter); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			return 0
		}
		return delay
	}

	return fallback
}

// SleepWithCtx sleeps for d or until ctx is cancelled, whichever comes first,
// returning ctx.Err() on cancellation.
func SleepWithCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
