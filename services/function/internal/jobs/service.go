package jobs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
)

var ErrNoSearchTerms = errors.New("at least one search term must be provided")

var defaultSearchTerms = []string{"golang", "python backend"}

type Service struct {
	logger  *slog.Logger
	scraper *LinkedInScraper
	mu      sync.Mutex
	store   *Store
}

func NewService(logger *slog.Logger) *Service {
	return &Service{
		logger:  logger,
		scraper: NewLinkedInScraper(logger),
	}
}

func (s *Service) NormalizeTerms(terms []string) []string {
	if len(terms) == 0 {
		return append([]string(nil), defaultSearchTerms...)
	}

	normalized := make([]string, 0, len(terms))
	for _, term := range terms {
		trimmed := strings.TrimSpace(term)
		if trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}

	if len(normalized) == 0 {
		return nil
	}

	return normalized
}

func (s *Service) StartCapture(requestID string, terms []string) error {
	normalized := s.NormalizeTerms(terms)
	if len(normalized) == 0 {
		return ErrNoSearchTerms
	}

	if _, err := s.ensureStore(context.Background()); err != nil {
		return err
	}

	go s.runCapture(requestID, normalized)
	return nil
}

func (s *Service) ensureStore(ctx context.Context) (*Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store != nil {
		return s.store, nil
	}

	store, err := NewStore(ctx)
	if err != nil {
		return nil, err
	}

	s.store = store
	return s.store, nil
}

func (s *Service) runCapture(requestID string, terms []string) {
	logger := s.logger.With("request_id", requestID)
	logger.Info("job capture started", "terms", terms)

	store, err := s.ensureStore(context.Background())
	if err != nil {
		logger.Error("job store initialization failed", "error", err)
		return
	}

	jobs, err := s.scraper.GetJobIDsFromLinkedIn(context.Background(), terms)
	if err != nil {
		logger.Error("job capture encountered errors", "error", err, "captured_jobs", len(jobs))
	}

	if len(jobs) == 0 {
		logger.Warn("job capture finished with no jobs captured")
		return
	}

	inserted, err := store.SaveJobsToDatabase(context.Background(), jobs)
	if err != nil {
		logger.Error("job persistence failed", "error", err)
		return
	}

	logger.Info("job capture finished", "captured_jobs", len(jobs), "inserted_jobs", inserted)
}