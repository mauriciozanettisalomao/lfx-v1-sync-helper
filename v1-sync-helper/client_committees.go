// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Committee-specific client operations for the v1-sync-helper service.
package main

import (
	"context"
	"fmt"

	committeeservice "github.com/linuxfoundation/lfx-v2-committee-service/gen/committee_service"
)

// fetchCommitteeBase fetches an existing committee base from the Committee Service API.
func fetchCommitteeBase(ctx context.Context, committeeUID string) (*committeeservice.CommitteeBaseWithReadonlyAttributes, string, error) {
	token, err := generateCachedJWTToken(committeeServiceAudience, UserInfo{})
	if err != nil {
		return nil, "", err
	}

	result, err := committeeClient.GetCommitteeBase(ctx, &committeeservice.GetCommitteeBasePayload{
		BearerToken: &token,
		UID:         &committeeUID,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch committee base: %w", err)
	}

	etag := ""
	if result.Etag != nil {
		etag = *result.Etag
	}

	return result.CommitteeBase, etag, nil
}

// createCommittee creates a new committee via the Committee Service API.
func createCommittee(ctx context.Context, payload *committeeservice.CreateCommitteePayload, userInfo UserInfo) (*committeeservice.CommitteeFullWithReadonlyAttributes, error) {
	token, err := generateCachedJWTToken(committeeServiceAudience, userInfo)
	if err != nil {
		return nil, err
	}

	payload.BearerToken = &token

	result, err := committeeClient.CreateCommittee(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create committee: %w", err)
	}

	return result, nil
}

// updateCommittee updates a committee by separately handling base and settings if there are changes.
func updateCommittee(ctx context.Context, payload *committeeservice.UpdateCommitteeBasePayload, userInfo UserInfo) error {
	// Fetch current committee base.
	currentBase, baseETag, err := fetchCommitteeBase(ctx, *payload.UID)
	if err != nil {
		return fmt.Errorf("failed to fetch current committee base: %w", err)
	}

	// Create updated base for comparison.
	updatedBase := &committeeservice.CommitteeBaseWithReadonlyAttributes{
		UID:         currentBase.UID,
		Name:        stringToStringPtr(payload.Name),
		ProjectUID:  stringToStringPtr(payload.ProjectUID),
		Category:    stringToStringPtr(payload.Category),
		Description: payload.Description,
		Website:     payload.Website,
	}

	// Check if base has changes.
	baseChanged := !committeeBasesEqual(currentBase, updatedBase)

	if baseChanged {
		token, err := generateCachedJWTToken(committeeServiceAudience, userInfo)
		if err != nil {
			return fmt.Errorf("failed to generate token for committee base update: %w", err)
		}

		payload.BearerToken = &token
		payload.IfMatch = stringToStringPtr(baseETag)

		_, err = committeeClient.UpdateCommitteeBase(ctx, payload)
		if err != nil {
			return fmt.Errorf("failed to update committee base: %w", err)
		}
	}

	// For now, assuming all fields are in base. If there are settings-specific fields,
	// we would handle them similarly here with fetchCommitteeSettings and UpdateCommitteeSettings.

	return nil
}

// committeeBasesEqual compares two CommitteeBaseWithReadonlyAttributes objects for equality.
func committeeBasesEqual(a, b *committeeservice.CommitteeBaseWithReadonlyAttributes) bool {
	return stringPtrToString(a.Name) == stringPtrToString(b.Name) &&
		stringPtrToString(a.ProjectUID) == stringPtrToString(b.ProjectUID) &&
		stringPtrToString(a.Category) == stringPtrToString(b.Category) &&
		stringPtrToString(a.Description) == stringPtrToString(b.Description) &&
		stringPtrToString(a.Website) == stringPtrToString(b.Website)
}
