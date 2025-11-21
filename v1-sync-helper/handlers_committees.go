// Committee-specific handlers for the v1-sync-helper service.
package main

import (
	"context"
	"fmt"

	committeeservice "github.com/linuxfoundation/lfx-v2-committee-service/gen/committee_service"
	"github.com/nats-io/nats.go/jetstream"
)

// handleCommitteeUpdate processes a committee update from the KV bucket.
func handleCommitteeUpdate(ctx context.Context, key string, v1Data map[string]any, userInfo UserInfo, mappingsKV jetstream.KeyValue) {
	// Check if we should skip this sync operation.
	if shouldSkipSync(ctx, v1Data) {
		return
	}

	// Extract committee SFID.
	sfid, ok := v1Data["sfid"].(string)
	if !ok || sfid == "" {
		logger.With("key", key).ErrorContext(ctx, "no SFID found in committee data")
		return
	}

	// Check if we have an existing mapping.
	mappingKey := fmt.Sprintf("committee.sfid.%s", sfid)
	existingUID := ""

	if entry, err := mappingsKV.Get(ctx, mappingKey); err == nil {
		existingUID = string(entry.Value())
	}

	var uid string
	var err error

	if existingUID != "" {
		// Update existing committee.
		logger.With("committee_uid", existingUID, "sfid", sfid).InfoContext(ctx, "updating existing committee")

		// Map V1 data to update payload.
		var payload *committeeservice.UpdateCommitteeBasePayload
		payload, err = mapV1DataToCommitteeUpdateBasePayload(ctx, existingUID, v1Data, mappingsKV)
		if err != nil {
			logger.With(errKey, err, "sfid", sfid).ErrorContext(ctx, "failed to map v1 data to update payload")
			return
		}

		err = updateCommittee(ctx, payload, userInfo)
		uid = existingUID
	} else {
		// Create new committee.
		logger.With("sfid", sfid).InfoContext(ctx, "creating new committee")

		// Map V1 data to create payload.
		var payload *committeeservice.CreateCommitteePayload
		payload, err = mapV1DataToCommitteeCreatePayload(ctx, v1Data, mappingsKV)
		if err != nil {
			logger.With(errKey, err, "sfid", sfid).ErrorContext(ctx, "failed to map v1 data to create payload")
			return
		}

		var response *committeeservice.CommitteeFullWithReadonlyAttributes
		response, err = createCommittee(ctx, payload, userInfo)
		if response != nil && response.UID != nil {
			uid = *response.UID
		}
	}

	if err != nil {
		logger.With(errKey, err, "sfid", sfid).ErrorContext(ctx, "failed to sync committee")
		return
	}

	// Store the mapping.
	if uid != "" {
		if _, err := mappingsKV.Put(ctx, mappingKey, []byte(uid)); err != nil {
			logger.With(errKey, err, "sfid", sfid, "uid", uid).WarnContext(ctx, "failed to store committee mapping")
		}
	}

	logger.With("committee_uid", uid, "sfid", sfid).InfoContext(ctx, "successfully synced committee")
}

// mapV1DataToCommitteeCreatePayload converts V1 committee data to a CreateCommitteePayload.
func mapV1DataToCommitteeCreatePayload(ctx context.Context, v1Data map[string]any, mappingsKV jetstream.KeyValue) (*committeeservice.CreateCommitteePayload, error) {
	// Extract required fields.
	name := ""
	if mailingList, ok := v1Data["mailing_list__c"].(string); ok {
		name = mailingList
	}

	projectUID := ""
	if projectSFID, ok := v1Data["project_name__c"].(string); ok && projectSFID != "" {
		// Look up the project's V2 UID from SFID mappings.
		projectMappingKey := fmt.Sprintf("project.sfid.%s", projectSFID)
		if entry, err := mappingsKV.Get(ctx, projectMappingKey); err == nil {
			projectUID = string(entry.Value())
			logger.With("project_sfid", projectSFID, "project_uid", projectUID).DebugContext(ctx, "found project UID from SFID mapping for committee")
		} else {
			logger.With("project_sfid", projectSFID, errKey, err).WarnContext(ctx, "could not find project UID in mappings for committee")
		}
	}

	if name == "" || projectUID == "" {
		return nil, fmt.Errorf("missing required fields: name=%q, projectUID=%q", name, projectUID)
	}

	payload := &committeeservice.CreateCommitteePayload{
		Name:       name,
		ProjectUID: projectUID,
	}

	// Map optional fields.
	if desc, ok := v1Data["description__c"].(string); ok && desc != "" {
		payload.Description = &desc
	}

	if typeVal, ok := v1Data["type__c"].(string); ok && typeVal != "" {
		payload.Category = typeVal // Map Type to Category.
	}

	if websiteURL, ok := v1Data["committee_website__c"].(string); ok && websiteURL != "" {
		payload.Website = &websiteURL
	}

	if enableVoting, ok := v1Data["enable_voting__c"].(bool); ok {
		payload.EnableVoting = enableVoting
	}

	if ssoEnabled, ok := v1Data["sso_group_enabled"].(bool); ok {
		payload.SsoGroupEnabled = ssoEnabled
	}

	return payload, nil
}

// mapV1DataToCommitteeUpdateBasePayload converts V1 committee data to an UpdateCommitteeBasePayload.
func mapV1DataToCommitteeUpdateBasePayload(ctx context.Context, committeeUID string, v1Data map[string]any, mappingsKV jetstream.KeyValue) (*committeeservice.UpdateCommitteeBasePayload, error) {
	// Extract required fields.
	name := ""
	if mailingList, ok := v1Data["mailing_list__c"].(string); ok {
		name = mailingList
	}

	projectUID := ""
	if projectSFID, ok := v1Data["project_name__c"].(string); ok && projectSFID != "" {
		// Look up the project's V2 UID from SFID mappings.
		projectMappingKey := fmt.Sprintf("project.sfid.%s", projectSFID)
		if entry, err := mappingsKV.Get(ctx, projectMappingKey); err == nil {
			projectUID = string(entry.Value())
			logger.With("project_sfid", projectSFID, "project_uid", projectUID).DebugContext(ctx, "found project UID from SFID mapping for committee")
		} else {
			logger.With("project_sfid", projectSFID, errKey, err).WarnContext(ctx, "could not find project UID in mappings for committee")
		}
	}

	if name == "" || projectUID == "" {
		return nil, fmt.Errorf("missing required fields: name=%q, projectUID=%q", name, projectUID)
	}

	payload := &committeeservice.UpdateCommitteeBasePayload{
		UID:        &committeeUID,
		Name:       name,
		ProjectUID: projectUID,
	}

	// Map optional fields.
	if desc, ok := v1Data["description__c"].(string); ok && desc != "" {
		payload.Description = &desc
	}

	if typeVal, ok := v1Data["type__c"].(string); ok && typeVal != "" {
		payload.Category = typeVal // Map Type to Category.
	}

	if websiteURL, ok := v1Data["committee_website__c"].(string); ok && websiteURL != "" {
		payload.Website = &websiteURL
	}

	return payload, nil
}
