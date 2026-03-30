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

func shouldRetry(statusCode int) bool {
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
			if !shouldRetry(resp.StatusCode) {
				break
			}
		}

		if i < maxRetries-1 {
			if err = sleepWithCtx(req.Context(), retryDelayForAttempt(resp, i)); err != nil {
				if resp != nil {
					resp.Body.Close()
				}
				return nil, fmt.Errorf("failed to sleep: %w", err)
			}
		}
	}
	return resp, err
}

func retryDelayForAttempt(resp *http.Response, attempt int) time.Duration {
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

func sleepWithCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
