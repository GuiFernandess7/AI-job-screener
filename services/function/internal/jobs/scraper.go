package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const linkedInGuestSearchURL = "https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search"
const linkedInRetryAttempts = 3
const linkedInRateLimitBaseDelay = 15 * time.Second

const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

var jobPostingCardKeyPattern = regexp.MustCompile(`jobPostingCardKey":"(\d+)"`)
var jobViewURLPattern = regexp.MustCompile(`/view/[^\"?]*?(\d{6,})`)

type CapturedJob struct {
	ID         string
	SearchTerm string
}

type LinkedInScraper struct {
	httpClient *http.Client
	logger     *slog.Logger
	rand       *rand.Rand
}

func NewLinkedInScraper(logger *slog.Logger) *LinkedInScraper {
	return &LinkedInScraper{
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   20 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
		logger:     logger,
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *LinkedInScraper) GetJobIDsFromLinkedIn(ctx context.Context, terms []string) ([]CapturedJob, error) {
	jobs := make([]CapturedJob, 0)
	seen := make(map[string]struct{})
	var failedTerms []string
	var errs []error
	rateLimited := false

	for index, term := range terms {
		s.logger.Info("linkedin term processing started", "term", term)
		termJobs, err := s.fetchTermJobs(ctx, term)
		if err != nil {
			if ctx.Err() != nil {
				return jobs, ctx.Err()
			}

			s.logger.Error("linkedin term processing failed", "term", term, "error", err)
			failedTerms = append(failedTerms, term)
			errs = append(errs, err)
			if isLinkedInRateLimitError(err) {
				rateLimited = true
			}
		} else {
			for _, job := range termJobs {
				key := job.ID + "|" + job.SearchTerm
				if _, exists := seen[key]; exists {
					continue
				}

				seen[key] = struct{}{}
				jobs = append(jobs, job)
			}

			s.logger.Info("linkedin term processing finished", "term", term, "jobs_found", len(termJobs))
		}

		if index < len(terms)-1 {
			// Jitter reduces the chance of repeating the same timing signature between requests.
			// Back off harder for the remaining terms once we've already been rate limited once.
			delay := time.Duration(4+s.rand.Intn(6)) * time.Second
			if rateLimited {
				delay += linkedInRateLimitBaseDelay
			}

			select {
			case <-ctx.Done():
				return jobs, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	if len(failedTerms) > 0 {
		s.logger.Warn("linkedin term processing partially failed", "failed_terms", failedTerms, "jobs_found", len(jobs))
		return jobs, fmt.Errorf("linkedin jobs request failed for terms %v: %w", failedTerms, errors.Join(errs...))
	}

	return jobs, nil
}

func (s *LinkedInScraper) fetchTermJobs(ctx context.Context, term string) ([]CapturedJob, error) {
	requestURL, err := buildLinkedInGuestSearchURL(term)
	if err != nil {
		return nil, err
	}

	s.logger.Info("linkedin request prepared", "term", term, "url", requestURL)

	var lastErr error
	for attempt := 1; attempt <= linkedInRetryAttempts; attempt++ {
		s.logger.Info("linkedin request started", "term", term, "attempt", attempt, "url", requestURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build linkedin request: %w", err)
		}
		req.Header.Set("User-Agent", browserUserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			lastErr = err
			s.logger.Warn("linkedin request failed", "term", term, "attempt", attempt, "url", requestURL, "error", err)
			if attempt < linkedInRetryAttempts && isRetryableLinkedInError(err) {
				s.logger.Info("linkedin request retry scheduled", "term", term, "attempt", attempt, "url", requestURL)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(attempt) * 2 * time.Second):
				}
				continue
			}

			return nil, fmt.Errorf("request linkedin jobs for %q: %w", term, err)
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read linkedin response body for %q: %w", term, readErr)
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("linkedin jobs request for %q returned status %d", term, resp.StatusCode)
			s.logger.Warn("linkedin request returned non-200 status", "term", term, "attempt", attempt, "url", requestURL, "status", resp.StatusCode)
			if attempt < linkedInRetryAttempts && isRetryableLinkedInStatus(resp.StatusCode) {
				delay := linkedInStatusRetryDelay(resp.StatusCode, attempt, resp.Header)
				s.logger.Info("linkedin status retry scheduled", "term", term, "attempt", attempt, "url", requestURL, "delay", delay)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
				continue
			}

			return nil, lastErr
		}

		jobs := parseJobIDs(string(body), term)
		s.logger.Info("linkedin request finished", "term", term, "attempt", attempt, "url", requestURL, "status", resp.StatusCode, "jobs_found", len(jobs))
		return jobs, nil
	}

	return nil, fmt.Errorf("request linkedin jobs for %q: %w", term, lastErr)
}

func isRetryableLinkedInStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func isLinkedInRateLimitError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 429")
}

func linkedInStatusRetryDelay(status int, attempt int, headers http.Header) time.Duration {
	if status == http.StatusTooManyRequests {
		if retryAfter, ok := parseRetryAfter(headers.Get("Retry-After")); ok {
			return retryAfter
		}

		return time.Duration(attempt) * linkedInRateLimitBaseDelay
	}

	return time.Duration(attempt) * 2 * time.Second
}

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}

	if when, err := time.Parse(http.TimeFormat, value); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			return 0, false
		}
		return delay, true
	}

	return 0, false
}

func isRetryableLinkedInError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "tls handshake timeout") || strings.Contains(errText, "timeout") || strings.Contains(errText, "temporary")
}

func buildLinkedInGuestSearchURL(term string) (string, error) {
	parsedURL, err := url.Parse(linkedInGuestSearchURL)
	if err != nil {
		return "", fmt.Errorf("parse linkedin guest api url: %w", err)
	}

	query := parsedURL.Query()
	query.Set("keywords", term)
	query.Set("location", "Brazil")
	query.Set("geoId", "106057199")
	query.Set("f_TPR", "r86400")
	query.Set("start", "0")
	parsedURL.RawQuery = query.Encode()

	return parsedURL.String(), nil
}

func parseJobIDs(body string, term string) []CapturedJob {
	matches := make([]CapturedJob, 0)
	seenIDs := make(map[string]struct{})

	collectMatch := func(match string) {
		jobID := strings.TrimSpace(match)
		if jobID == "" {
			return
		}
		if _, exists := seenIDs[jobID]; exists {
			return
		}

		seenIDs[jobID] = struct{}{}
		matches = append(matches, CapturedJob{ID: jobID, SearchTerm: term})
	}

	for _, groups := range jobPostingCardKeyPattern.FindAllStringSubmatch(body, -1) {
		if len(groups) > 1 {
			collectMatch(groups[1])
		}
	}

	for _, groups := range jobViewURLPattern.FindAllStringSubmatch(body, -1) {
		if len(groups) > 1 {
			collectMatch(groups[1])
		}
	}

	return matches
}