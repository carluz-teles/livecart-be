package store

import (
	"context"
	"fmt"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, input CreateStoreInput) (CreateStoreOutput, error) {
	existing, err := s.repo.GetBySlug(ctx, input.Slug)
	if err != nil && !httpx.IsNotFound(err) {
		return CreateStoreOutput{}, fmt.Errorf("checking slug uniqueness: %w", err)
	}
	if existing != nil {
		return CreateStoreOutput{}, httpx.ErrConflict("slug already in use")
	}

	row, err := s.repo.Create(ctx, CreateStoreParams{
		Name: input.Name,
		Slug: input.Slug,
	})
	if err != nil {
		return CreateStoreOutput{}, fmt.Errorf("creating store: %w", err)
	}

	return CreateStoreOutput{
		ID:        row.ID,
		Name:      row.Name,
		Slug:      row.Slug,
		CreatedAt: row.CreatedAt,
	}, nil
}

func (s *Service) GetByID(ctx context.Context, id string) (StoreOutput, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(*row), nil
}

func (s *Service) Update(ctx context.Context, input UpdateStoreInput) (StoreOutput, error) {
	row, err := s.repo.Update(ctx, UpdateStoreParams{
		ID:             input.StoreID,
		Name:           input.Name,
		WhatsappNumber: input.WhatsappNumber,
		EmailAddress:   input.EmailAddress,
		SMSNumber:      input.SMSNumber,
	})
	if err != nil {
		return StoreOutput{}, err
	}

	return toStoreOutput(row), nil
}

func toStoreOutput(row StoreRow) StoreOutput {
	return StoreOutput{
		ID:             row.ID,
		Name:           row.Name,
		Slug:           row.Slug,
		Active:         row.Active,
		WhatsappNumber: row.WhatsappNumber,
		EmailAddress:   row.EmailAddress,
		SMSNumber:      row.SMSNumber,
		CreatedAt:      row.CreatedAt,
	}
}
