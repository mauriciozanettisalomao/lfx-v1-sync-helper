// The v1-sync-helper service.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
)

// getRootProjectUID makes a NATS request to get the ROOT project UID.
func getRootProjectUID(ctx context.Context) (string, error) {
	// Create context with timeout for the NATS request.
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	logger.DebugContext(ctx, "requesting ROOT project UID via NATS")

	// Make a NATS request to the slug_to_uid subject.
	resp, err := natsConn.RequestWithContext(requestCtx, "lfx.projects-api.slug_to_uid", []byte("ROOT"))
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to request ROOT project UID")
		return "", fmt.Errorf("failed to request ROOT project UID: %w", err)
	}

	// The response should be the UUID string.
	rootUID := strings.TrimSpace(string(resp.Data))
	if rootUID == "" {
		logger.ErrorContext(ctx, "received empty ROOT project UID response")
		return "", fmt.Errorf("empty ROOT project UID response")
	}

	logger.With("root_uid", rootUID).DebugContext(ctx, "successfully retrieved ROOT project UID")
	return rootUID, nil
}

// extractUserInfo extracts user information from V1 data for API calls and JWT impersonation.
func extractUserInfo(ctx context.Context, v1Data map[string]any, mappingsKV jetstream.KeyValue) UserInfo {
	// Extract platform ID from lastmodifiedbyid
	if lastModifiedBy, ok := v1Data["lastmodifiedbyid"].(string); ok && lastModifiedBy != "" {
		// Check if this is a machine user with @clients suffix
		if strings.HasSuffix(lastModifiedBy, "@clients") {
			// Machine user - pass through with @clients only on principal
			return UserInfo{
				Username:  strings.TrimSuffix(lastModifiedBy, "@clients"), // Subject without @clients
				Email:     "",                                             // No email for machine users
				Principal: lastModifiedBy,                                 // Principal includes @clients
			}
		}

		// Regular platform ID - look up via v1 API
		userInfo, err := getUserInfoFromV1(ctx, lastModifiedBy, mappingsKV)
		if err != nil || userInfo.Username == "" {
			logger.With(errKey, err, "platform_id", lastModifiedBy).WarnContext(ctx, "failed to get user info from v1 API, falling back to service account")
			return UserInfo{} // Empty UserInfo triggers fallback to v1_sync_helper@clients
		}

		return userInfo
	}
	return UserInfo{}
}

// kvHandler processes KV bucket updates from Meltano
func kvHandler(entry jetstream.KeyValueEntry, v1KV jetstream.KeyValue, mappingsKV jetstream.KeyValue) {
	ctx := context.Background()

	key := entry.Key()
	operation := entry.Operation()

	logger.With("key", key, "operation", operation.String()).DebugContext(ctx, "processing KV entry")

	// Handle different operations
	switch operation {
	case jetstream.KeyValuePut:
		handleKVPut(ctx, entry, v1KV, mappingsKV)
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		handleKVDelete(ctx, entry, v1KV, mappingsKV)
	default:
		logger.With("key", key, "operation", operation.String()).DebugContext(ctx, "ignoring KV operation")
	}
}

// handleKVPut processes a KV put operation (create/update)
func handleKVPut(ctx context.Context, entry jetstream.KeyValueEntry, v1KV jetstream.KeyValue, mappingsKV jetstream.KeyValue) {
	key := entry.Key()

	// Parse the JSON data
	var v1Data map[string]any
	if err := json.Unmarshal(entry.Value(), &v1Data); err != nil {
		logger.With(errKey, err, "key", key).ErrorContext(ctx, "failed to unmarshal KV entry data")
		return
	}

	// Extract user information for API calls and JWT impersonation
	userInfo := extractUserInfo(ctx, v1Data, mappingsKV)

	// Determine the object type based on the key pattern
	if strings.HasPrefix(key, "salesforce-project__c.") {
		handleProjectUpdate(ctx, key, v1Data, userInfo, mappingsKV)
	} else if strings.HasPrefix(key, "platform-collaboration__c.") {
		handleCommitteeUpdate(ctx, key, v1Data, userInfo, mappingsKV)
	} else {
		logger.With("key", key).DebugContext(ctx, "unknown object type, ignoring")
	}
}

// handleKVDelete processes a KV delete operation
func handleKVDelete(ctx context.Context, entry jetstream.KeyValueEntry, v1KV jetstream.KeyValue, mappingsKV jetstream.KeyValue) {
	key := entry.Key()

	// For deletes, we would need to look up the mapping and call delete APIs
	// This is a simplified implementation
	logger.With("key", key).InfoContext(ctx, "delete operation not yet implemented")
}

// handleProjectUpdate processes a project update from the KV bucket
func handleProjectUpdate(ctx context.Context, key string, v1Data map[string]any, userInfo UserInfo, mappingsKV jetstream.KeyValue) {
	// Extract project SFID (primary key)
	sfid, ok := v1Data["sfid"].(string)
	if !ok || sfid == "" {
		logger.With("key", key).ErrorContext(ctx, "no SFID found in project data")
		return
	}

	// Extract project slug for additional mapping
	slug, _ := v1Data["slug__c"].(string)

	// Check if we have an existing mapping using SFID
	mappingKey := fmt.Sprintf("project.sfid.%s", sfid)
	existingUID := ""

	if entry, err := mappingsKV.Get(ctx, mappingKey); err == nil {
		existingUID = string(entry.Value())
	}

	// Map V1 data to Project request
	projectReq, err := mapV1DataToProjectRequest(ctx, existingUID, v1Data, mappingsKV)
	if err != nil {
		logger.With(errKey, err, "sfid", sfid, "slug", slug).ErrorContext(ctx, "failed to map project data")
		return
	}

	var response *ProjectResponse

	if existingUID != "" {
		// Update existing project
		logger.With("project_uid", existingUID, "sfid", sfid, "slug", slug).InfoContext(ctx, "updating existing project")
		response, err = updateProject(ctx, existingUID, projectReq, userInfo)
	} else {
		// Create new project
		logger.With("sfid", sfid, "slug", slug).InfoContext(ctx, "creating new project")
		response, err = createProject(ctx, projectReq, userInfo)
	}

	if err != nil {
		logger.With(errKey, err, "sfid", sfid, "slug", slug).ErrorContext(ctx, "failed to sync project")
		return
	}

	// Store the SFID mapping
	if _, err := mappingsKV.Put(ctx, mappingKey, []byte(response.UID)); err != nil {
		logger.With(errKey, err, "sfid", sfid, "uid", response.UID).WarnContext(ctx, "failed to store project mapping")
	}

	logger.With("project_uid", response.UID, "sfid", sfid, "slug", slug).InfoContext(ctx, "successfully synced project")
}

// handleCommitteeUpdate processes a committee update from the KV bucket
func handleCommitteeUpdate(ctx context.Context, key string, v1Data map[string]any, userInfo UserInfo, mappingsKV jetstream.KeyValue) {
	// Extract committee SFID
	sfid, ok := v1Data["sfid"].(string)
	if !ok || sfid == "" {
		logger.With("key", key).ErrorContext(ctx, "no SFID found in committee data")
		return
	}

	// Check if we have an existing mapping
	mappingKey := fmt.Sprintf("committee.sfid.%s", sfid)
	existingUID := ""

	if entry, err := mappingsKV.Get(ctx, mappingKey); err == nil {
		existingUID = string(entry.Value())
	}

	// Map V1 data to Committee request
	committeeReq := mapV1DataToCommitteeRequest(ctx, existingUID, v1Data, mappingsKV)

	var response *CommitteeResponse
	var err error

	if existingUID != "" {
		// Update existing committee
		logger.With("committee_uid", existingUID, "sfid", sfid).InfoContext(ctx, "updating existing committee")
		response, err = updateCommittee(ctx, existingUID, committeeReq, userInfo)
	} else {
		// Create new committee
		logger.With("sfid", sfid).InfoContext(ctx, "creating new committee")
		response, err = createCommittee(ctx, committeeReq, userInfo)
	}

	if err != nil {
		logger.With(errKey, err, "sfid", sfid).ErrorContext(ctx, "failed to sync committee")
		return
	}

	// Store the mapping
	if _, err := mappingsKV.Put(ctx, mappingKey, []byte(response.UID)); err != nil {
		logger.With(errKey, err, "sfid", sfid, "uid", response.UID).WarnContext(ctx, "failed to store committee mapping")
	}

	logger.With("committee_uid", response.UID, "sfid", sfid).InfoContext(ctx, "successfully synced committee")
}

// mapV1DataToProjectRequest converts V1 project data to a ProjectRequest
func mapV1DataToProjectRequest(ctx context.Context, existingUID string, v1Data map[string]any, mappingsKV jetstream.KeyValue) (ProjectRequest, error) {
	project := ProjectRequest{}

	if existingUID != "" {
		project.UID = existingUID
	}

	// Map fields from V1 data
	if name, ok := v1Data["name"].(string); ok {
		project.Name = name
	}

	if slug, ok := v1Data["slug__c"].(string); ok {
		project.Slug = slug
	}

	if desc, ok := v1Data["description__c"].(string); ok {
		project.Description = desc
	}

	if public, ok := v1Data["public__c"].(bool); ok {
		project.Public = public
	}

	if formationDate, ok := v1Data["formation_date__c"].(string); ok {
		project.FormationDate = formationDate
	}

	if legalEntityName, ok := v1Data["legal_entity_name__c"].(string); ok {
		project.LegalEntityName = legalEntityName
	}

	if legalEntityType, ok := v1Data["legal_entity_type__c"].(string); ok {
		project.LegalEntityType = legalEntityType
	}

	if logoURL, ok := v1Data["logo_url__c"].(string); ok {
		project.LogoURL = logoURL
	}

	if repoURL, ok := v1Data["repository_url__c"].(string); ok {
		project.RepositoryURL = repoURL
	}

	if stage, ok := v1Data["stage__c"].(string); ok {
		project.Stage = stage
	}

	if websiteURL, ok := v1Data["website_url__c"].(string); ok {
		project.WebsiteURL = websiteURL
	}

	// Handle parent project logic.
	parentProjectID := ""
	if parentID, ok := v1Data["parent_project__c"].(string); ok {
		parentProjectID = strings.TrimSpace(parentID)
	}

	if parentProjectID != "" {
		// Project has a parent in V1, look up the parent's V2 UID from SFID mappings.
		parentMappingKey := fmt.Sprintf("project.sfid.%s", parentProjectID)
		if entry, err := mappingsKV.Get(ctx, parentMappingKey); err == nil {
			project.ParentUID = string(entry.Value())
			logger.With("parent_project_sfid", parentProjectID, "parent_uid", project.ParentUID).DebugContext(ctx, "found parent project UID from SFID mapping")
		} else {
			logger.With("parent_project_sfid", parentProjectID, errKey, err).WarnContext(ctx, "could not find parent project UID in mappings, leaving ParentUID empty")
		}
	} else {
		// Project has no parent in V1, so it should be a child of ROOT in V2.
		rootUID, err := getRootProjectUID(ctx)
		if err != nil {
			return project, fmt.Errorf("failed to get ROOT project UID: %w", err)
		}
		project.ParentUID = rootUID
		logger.With("root_uid", rootUID).DebugContext(ctx, "set project parent to ROOT")
	}

	// Set timestamps
	if createdDate, ok := v1Data["createddate"].(string); ok {
		if parsedTime, err := time.Parse(time.RFC3339, createdDate); err == nil {
			project.CreatedAt = parsedTime
		} else {
			project.CreatedAt = time.Now().UTC()
		}
	} else {
		project.CreatedAt = time.Now().UTC()
	}

	if modifiedDate, ok := v1Data["lastmodifieddate"].(string); ok {
		if parsedTime, err := time.Parse(time.RFC3339, modifiedDate); err == nil {
			project.UpdatedAt = parsedTime
		}
	}

	return project, nil
}

// mapV1DataToCommitteeRequest converts V1 committee data to a CommitteeRequest
func mapV1DataToCommitteeRequest(ctx context.Context, existingUID string, v1Data map[string]any, mappingsKV jetstream.KeyValue) CommitteeRequest {
	committee := CommitteeRequest{}

	if existingUID != "" {
		committee.UID = existingUID
	} else {
		// Generate new UUID if creating
		if newUUID, err := uuid.NewV7(); err == nil {
			committee.UID = newUUID.String()
		}
	}

	// Extract mailing list name as the committee name
	if mailingList, ok := v1Data["mailing_list__c"].(string); ok {
		committee.Name = mailingList
	}

	// Extract the project UID reference from project_name__c (which contains project SFID)
	if projectSFID, ok := v1Data["project_name__c"].(string); ok && projectSFID != "" {
		// Look up the project's V2 UID from SFID mappings
		projectMappingKey := fmt.Sprintf("project.sfid.%s", projectSFID)
		if entry, err := mappingsKV.Get(ctx, projectMappingKey); err == nil {
			committee.ProjectUID = string(entry.Value())
			logger.With("project_sfid", projectSFID, "project_uid", committee.ProjectUID).DebugContext(ctx, "found project UID from SFID mapping for committee")
		} else {
			logger.With("project_sfid", projectSFID, errKey, err).WarnContext(ctx, "could not find project UID in mappings for committee")
		}
	}

	// Extract description
	if desc, ok := v1Data["description__c"].(string); ok {
		committee.Description = desc
	}

	// Extract voting flag
	if enableVoting, ok := v1Data["enable_voting__c"].(bool); ok {
		committee.EnableVoting = enableVoting
	}

	// Extract audit flag
	if isAudit, ok := v1Data["is_audit"].(bool); ok {
		committee.IsAudit = isAudit
	}

	// Extract type
	if typeVal, ok := v1Data["type__c"].(string); ok {
		committee.Type = typeVal
	}

	// Extract public flag and name
	if publicEnabled, ok := v1Data["public_enabled"].(bool); ok {
		committee.Public = publicEnabled
	}
	if publicName, ok := v1Data["public_name"].(string); ok {
		committee.PublicName = publicName
	}

	// Extract committee ID
	if committeeID, ok := v1Data["committee_id"].(string); ok {
		committee.CommitteeID = committeeID
	}

	// Extract website URL
	if websiteURL, ok := v1Data["committee_website__c"].(string); ok {
		committee.WebsiteURL = websiteURL
	}

	// Extract SSO group settings
	if ssoEnabled, ok := v1Data["sso_group_enabled"].(bool); ok {
		committee.SSOGroupEnabled = ssoEnabled
	}
	if ssoName, ok := v1Data["sso_group_name"].(string); ok {
		committee.SSOGroupName = ssoName
	}

	// Set timestamps
	if createdDate, ok := v1Data["createddate"].(string); ok {
		if parsedTime, err := time.Parse(time.RFC3339, createdDate); err == nil {
			committee.CreatedAt = parsedTime
		} else {
			committee.CreatedAt = time.Now().UTC()
		}
	} else {
		committee.CreatedAt = time.Now().UTC()
	}

	if modifiedDate, ok := v1Data["lastmodifieddate"].(string); ok {
		if parsedTime, err := time.Parse(time.RFC3339, modifiedDate); err == nil {
			committee.UpdatedAt = parsedTime
		}
	}

	return committee
}
