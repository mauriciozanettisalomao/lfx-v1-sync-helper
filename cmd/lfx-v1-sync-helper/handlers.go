// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The lfx-v1-sync-helper service.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// shouldSkipSync checks if the record was last modified by this service and
// should be skipped, because it originated in v2, and therefore does not need
// to be synced from v1.
func shouldSkipSync(ctx context.Context, v1Data map[string]any) bool {
	if lastModifiedBy, ok := v1Data["lastmodifiedbyid"].(string); ok && lastModifiedBy != "" {
		// Check if the lastmodifiedbyid matches our Auth0 Client ID with @clients suffix.
		ourServiceID := cfg.Auth0ClientID + "@clients"
		if lastModifiedBy == ourServiceID {
			logger.With("lastmodifiedbyid", lastModifiedBy).DebugContext(ctx, "skipping record that originated in v2")
			return true
		}
	}
	return false
}

// kvHandler processes KV bucket updates from Meltano
func kvHandler(entry jetstream.KeyValueEntry) {
	ctx := context.Background()

	key := entry.Key()
	operation := entry.Operation()

	logger.With("key", key, "operation", operation.String()).DebugContext(ctx, "processing KV entry")

	// Handle different operations
	switch operation {
	case jetstream.KeyValuePut:
		handleKVPut(ctx, entry)
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		handleKVDelete(ctx, entry)
	default:
		logger.With("key", key, "operation", operation.String()).DebugContext(ctx, "ignoring KV operation")
	}
}

// handleKVPut processes a KV put operation (create/update)
func handleKVPut(ctx context.Context, entry jetstream.KeyValueEntry) {
	key := entry.Key()

	// Parse the JSON data
	var v1Data map[string]any
	if err := json.Unmarshal(entry.Value(), &v1Data); err != nil {
		logger.With(errKey, err, "key", key).ErrorContext(ctx, "failed to unmarshal KV entry data")
		return
	}

	// Check if we should skip this sync operation.
	if shouldSkipSync(ctx, v1Data) {
		return
	}

	// Extract the prefix (everything before the first period) for faster lookup.
	prefix := key
	if dotIndex := strings.Index(key, "."); dotIndex != -1 {
		prefix = key[:dotIndex]
	}

	// Determine the object type based on the key prefix.
	switch prefix {
	case "salesforce-project__c":
		handleProjectUpdate(ctx, key, v1Data)
	case "platform-collaboration__c":
		handleCommitteeUpdate(ctx, key, v1Data)
	case "platform-community__c":
		handleCommitteeMemberUpdate(ctx, key, v1Data)
	case "itx-zoom-meetings-v2":
		handleZoomMeetingUpdate(ctx, key, v1Data)
	case "itx-zoom-meetings-registrants-v2":
		handleZoomMeetingRegistrantUpdate(ctx, key, v1Data)
	case "itx-zoom-meetings-mappings-v2":
		handleZoomMeetingMappingUpdate(ctx, key, v1Data)
	case "itx-zoom-past-meetings-attendees":
		handleZoomPastMeetingAttendeeUpdate(ctx, key, v1Data)
	case "itx-zoom-past-meetings-invitees":
		handleZoomPastMeetingInviteeUpdate(ctx, key, v1Data)
	case "itx-zoom-past-meetings-mappings":
		handleZoomPastMeetingMappingUpdate(ctx, key, v1Data)
	case "itx-zoom-past-meetings-recordings":
		handleZoomPastMeetingRecordingUpdate(ctx, key, v1Data)
	case "itx-zoom-past-meetings-summaries":
		handleZoomPastMeetingSummaryUpdate(ctx, key, v1Data)
	case "itx-zoom-past-meetings":
		handleZoomPastMeetingUpdate(ctx, key, v1Data)
	case "salesforce-merged_user":
		logger.With("key", key).DebugContext(ctx, "salesforce-merged_user sync not yet implemented")
	case "salesforce-alternate_email__c":
		handleAlternateEmailUpdate(ctx, key, v1Data)
	default:
		logger.With("key", key).WarnContext(ctx, "unknown object type, ignoring")
	}
}

// handleKVDelete processes a KV delete operation
func handleKVDelete(ctx context.Context, entry jetstream.KeyValueEntry) {
	key := entry.Key()

	// For deletes, we would need to look up the mapping and call delete APIs
	// This is a simplified implementation
	logger.With("key", key).InfoContext(ctx, "delete operation not yet implemented")
}

// extractDateOnly extracts the date part from an ISO 8601 datetime string.
// Input: "2020-03-01T00:00:00+00:00"
// Output: "2020-03-01"
func extractDateOnly(dateTimeStr string) string {
	if dateTimeStr == "" {
		return ""
	}

	// Extract just the date part from ISO 8601 datetime format.
	if datePart := strings.Split(dateTimeStr, "T")[0]; datePart != "" {
		return datePart
	}

	return ""
}

// handleAlternateEmailUpdate processes alternate email updates and maintains
// v1-mapping records for merged users' alternate emails.
func handleAlternateEmailUpdate(ctx context.Context, key string, v1Data map[string]any) {
	// Extract the leadorcontactid which references the sfid of merged_user table.
	leadorcontactid, ok := v1Data["leadorcontactid"].(string)
	if !ok || leadorcontactid == "" {
		logger.With("key", key).WarnContext(ctx, "alternate email missing leadorcontactid, skipping")
		return
	}

	// Extract the sfid of this alternate email record.
	emailSfid, ok := v1Data["sfid"].(string)
	if !ok || emailSfid == "" {
		logger.With("key", key).WarnContext(ctx, "alternate email missing sfid, skipping")
		return
	}

	// Check if this email is deleted.
	isDeleted := false
	if deletedVal, ok := v1Data["isdeleted"].(bool); ok {
		isDeleted = deletedVal
	}

	// Process the update in a goroutine to avoid blocking other handlers.
	go func() {
		updateUserAlternateEmails(ctx, leadorcontactid, emailSfid, isDeleted)
	}()
}

// updateUserAlternateEmails updates the v1-mapping record for a user's alternate emails
// with concurrency control using atomic KV operations.
func updateUserAlternateEmails(ctx context.Context, userSfid, emailSfid string, isDeleted bool) {
	mappingKey := fmt.Sprintf("v1-merged-user.alternate-emails.%s", userSfid)
	maxRetries := 5

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Add random splay time up to 1 second to reduce collision chances.
		splayTime := time.Duration(rand.Intn(1000)) * time.Millisecond
		time.Sleep(splayTime)

		// Get current mapping record.
		entry, err := mappingsKV.Get(ctx, mappingKey)

		var currentEmails []string
		var revision uint64

		if err != nil {
			if err == jetstream.ErrKeyNotFound {
				// Key doesn't exist, we'll create it.
				currentEmails = []string{}
				revision = 0
			} else {
				logger.With("error", err, "key", mappingKey, "attempt", attempt).
					ErrorContext(ctx, "failed to get mapping record")
				if attempt == maxRetries {
					return
				}
				continue
			}
		} else {
			// Parse existing emails list.
			revision = entry.Revision()
			if err := json.Unmarshal(entry.Value(), &currentEmails); err != nil {
				logger.With("error", err, "key", mappingKey).
					ErrorContext(ctx, "failed to unmarshal existing emails list")
				return
			}
		}

		// Update the emails list.
		updatedEmails := updateEmailsList(currentEmails, emailSfid, isDeleted)

		// Marshal the updated list.
		updatedData, err := json.Marshal(updatedEmails)
		if err != nil {
			logger.With("error", err, "key", mappingKey).
				ErrorContext(ctx, "failed to marshal updated emails list")
			return
		}

		// Attempt to save with concurrency control.
		var saveErr error
		if revision == 0 {
			// Try to create new record.
			_, saveErr = mappingsKV.Create(ctx, mappingKey, updatedData)
			if saveErr == jetstream.ErrKeyExists {
				// Key was created by another process, retry.
				logger.With("key", mappingKey, "attempt", attempt).
					DebugContext(ctx, "key created by another process during create attempt, retrying")
				continue
			}
		} else {
			// Try to update existing record.
			_, saveErr = mappingsKV.Update(ctx, mappingKey, updatedData, revision)
			if saveErr != nil {
				// Update failed (likely revision mismatch), retry.
				logger.With("error", saveErr, "key", mappingKey, "attempt", attempt).
					DebugContext(ctx, "update failed, retrying")
				continue
			}
		}

		if saveErr == nil {
			// Success!
			logger.With("key", mappingKey, "emailSfid", emailSfid, "isDeleted", isDeleted, "attempt", attempt).
				DebugContext(ctx, "successfully updated alternate emails mapping")
			return
		}

		// If we get here, there was an unexpected error.
		logger.With("error", saveErr, "key", mappingKey, "attempt", attempt).
			ErrorContext(ctx, "unexpected error during save operation")

		if attempt == maxRetries {
			logger.With("key", mappingKey, "maxRetries", maxRetries).
				ErrorContext(ctx, "max retries exceeded for updating alternate emails mapping")
			return
		}
	}
}

// updateEmailsList adds or removes an email sfid from the list based on deletion status.
func updateEmailsList(currentEmails []string, emailSfid string, isDeleted bool) []string {
	// Find if the email already exists in the list.
	index := -1
	for i, email := range currentEmails {
		if email == emailSfid {
			index = i
			break
		}
	}

	if isDeleted {
		// Remove from list if it exists.
		if index != -1 {
			// Remove element at index.
			return append(currentEmails[:index], currentEmails[index+1:]...)
		}
		// Email not in list, nothing to remove.
		return currentEmails
	} else {
		// Add to list if it doesn't exist.
		if index == -1 {
			return append(currentEmails, emailSfid)
		}
		// Email already in list, nothing to add.
		return currentEmails
	}
}
