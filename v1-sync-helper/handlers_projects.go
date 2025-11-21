// Project-specific handlers for the v1-sync-helper service.
package main

import (
	"context"
	"fmt"
	"strings"

	projectservice "github.com/linuxfoundation/lfx-v2-project-service/api/project/v1/gen/project_service"
	"github.com/nats-io/nats.go/jetstream"
)

// isValidURL checks if a URL value is non-empty and not "nil".
func isValidURL(url string) bool {
	trimmed := strings.TrimSpace(url)
	return trimmed != "" && trimmed != "nil"
}

// cleanURL trims whitespace and adds https:// prefix if no protocol is specified.
func cleanURL(url string) string {
	clean := strings.TrimSpace(url)
	if !strings.HasPrefix(clean, "http://") && !strings.HasPrefix(clean, "https://") {
		clean = "https://" + clean
	}
	return clean
}

// allowedCategories defines the valid values for admin_category__c mapping to category.
var allowedCategories = map[string]bool{
	"Active":         true,
	"Adopted":        true,
	"Archived":       true,
	"At-Large":       true,
	"Early Adoption": true,
	"Emeritus":       true,
	"Graduated":      true,
	"Growth":         true,
	"Idle":           true,
	"Impact":         true,
	"Incubating":     true,
	"Kanister":       true,
	"Mature":         true,
	"Pre-LFESS":      true,
	"Sandbox":        true,
	"SIG":            true,
	"Standards":      true,
	"TAC":            true,
	"Working Group":  true,
	"TAG":            true,
	"NONE":           true,
}

// mapAdminCategoryToCategory filters and maps admin_category__c to category.
func mapAdminCategoryToCategory(adminCategory string) *string {
	if adminCategory == "" {
		return nil
	}

	if allowedCategories[adminCategory] {
		return &adminCategory
	}

	// If the value is not in the allowed list, use NONE as fallback.
	fallback := "NONE"
	return &fallback
}

// handleProjectUpdate processes a project update from the KV bucket.
func handleProjectUpdate(ctx context.Context, key string, v1Data map[string]any, userInfo UserInfo, mappingsKV jetstream.KeyValue) {
	// Check if we should skip this sync operation.
	if shouldSkipSync(ctx, v1Data) {
		return
	}

	// Extract project SFID (primary key).
	sfid, ok := v1Data["sfid"].(string)
	if !ok || sfid == "" {
		logger.With("key", key).ErrorContext(ctx, "no SFID found in project data")
		return
	}

	// Extract project slug for additional mapping.
	slug, _ := v1Data["slug__c"].(string)

	// Check if we have an existing mapping using SFID.
	mappingKey := fmt.Sprintf("project.sfid.%s", sfid)
	existingUID := ""

	if entry, err := mappingsKV.Get(ctx, mappingKey); err == nil {
		existingUID = string(entry.Value())
	}

	var uid string
	var err error

	if existingUID != "" {
		// Update existing project.
		logger.With("project_uid", existingUID, "sfid", sfid, "slug", slug).InfoContext(ctx, "updating existing project")

		// Map V1 data to update payload.
		var payload *projectservice.UpdateProjectBasePayload
		payload, err = mapV1DataToProjectUpdateBasePayload(ctx, existingUID, v1Data, mappingsKV)
		if err != nil {
			logger.With(errKey, err, "sfid", sfid, "slug", slug).ErrorContext(ctx, "failed to map v1 data to update payload")
			return
		}

		// Map V1 data to settings payload.
		var settingsPayload *projectservice.UpdateProjectSettingsPayload
		settingsPayload, err = mapV1DataToProjectUpdateSettingsPayload(ctx, existingUID, v1Data)
		if err != nil {
			logger.With(errKey, err, "sfid", sfid, "slug", slug).ErrorContext(ctx, "failed to map v1 data to settings payload")
			return
		}

		err = updateProject(ctx, payload, settingsPayload, userInfo)
		uid = existingUID
	} else {
		// Create new project.
		logger.With("sfid", sfid, "slug", slug).InfoContext(ctx, "creating new project")

		// Map V1 data to create payload.
		var payload *projectservice.CreateProjectPayload
		payload, err = mapV1DataToProjectCreatePayload(ctx, v1Data, mappingsKV)
		if err != nil {
			logger.With(errKey, err, "sfid", sfid, "slug", slug).ErrorContext(ctx, "failed to map v1 data to create payload")
			return
		}

		var response *projectservice.ProjectFull
		response, err = createProject(ctx, payload, userInfo)
		if response != nil && response.UID != nil {
			uid = *response.UID
		}
	}

	if err != nil {
		logger.With(errKey, err, "sfid", sfid, "slug", slug).ErrorContext(ctx, "failed to sync project")
		return
	}

	// Store the SFID mapping.
	if uid != "" {
		if _, err := mappingsKV.Put(ctx, mappingKey, []byte(uid)); err != nil {
			logger.With(errKey, err, "sfid", sfid, "uid", uid).WarnContext(ctx, "failed to store project mapping")
		}
	}

	logger.With("project_uid", uid, "sfid", sfid, "slug", slug).InfoContext(ctx, "successfully synced project")
}

// mapV1DataToProjectCreatePayload converts V1 project data to a CreateProjectPayload.
func mapV1DataToProjectCreatePayload(ctx context.Context, v1Data map[string]any, mappingsKV jetstream.KeyValue) (*projectservice.CreateProjectPayload, error) {
	// Extract required fields.
	name, nameOK := v1Data["name"].(string)
	slug, slugOK := v1Data["slug__c"].(string)
	if !nameOK || !slugOK {
		return nil, fmt.Errorf("missing required fields: name=%t, slug=%t", nameOK, slugOK)
	}

	payload := &projectservice.CreateProjectPayload{
		Name: name,
		Slug: strings.ToLower(slug),
	}

	// Map base fields.
	if desc, ok := v1Data["description__c"].(string); ok && desc != "" {
		payload.Description = desc
	}

	if public, ok := v1Data["public__c"].(bool); ok {
		payload.Public = &public
	}

	// Map category from admin_category__c with filtering.
	if adminCategory, ok := v1Data["admin_category__c"].(string); ok {
		payload.Category = mapAdminCategoryToCategory(adminCategory)
	}

	// Map legal entity type from category__c.
	if category, ok := v1Data["category__c"].(string); ok && category != "" {
		payload.LegalEntityType = &category
	}

	// Map funding model (split semicolon-delimited values).
	if model, ok := v1Data["model__c"].(string); ok && model != "" {
		models := strings.Split(model, ";")
		for i, m := range models {
			models[i] = strings.TrimSpace(m)
		}
		payload.FundingModel = models
	}

	// Map charter URL.
	if charterURL, ok := v1Data["charterurl__c"].(string); ok && isValidURL(charterURL) {
		clean := cleanURL(charterURL)
		payload.CharterURL = &clean
	}

	// Map autojoin enabled.
	if autoJoin, ok := v1Data["auto_join_enabled__c"].(bool); ok {
		payload.AutojoinEnabled = &autoJoin
	}

	// Map formation date (start_date__c).
	if startDate, ok := v1Data["start_date__c"].(string); ok && startDate != "" {
		// Extract just the date part from ISO 8601 datetime format.
		if dateOnly := strings.Split(startDate, "T")[0]; dateOnly != "" {
			payload.FormationDate = &dateOnly
		}
	}

	// Map project logo.
	if logo, ok := v1Data["project_logo__c"].(string); ok && isValidURL(logo) {
		clean := cleanURL(logo)
		payload.LogoURL = &clean
	}

	// Map repository URL.
	if repoURL, ok := v1Data["repositoryurl__c"].(string); ok && isValidURL(repoURL) {
		clean := cleanURL(repoURL)
		payload.RepositoryURL = &clean
	}

	// Map website URL.
	if websiteURL, ok := v1Data["website__c"].(string); ok && isValidURL(websiteURL) {
		clean := cleanURL(websiteURL)
		payload.WebsiteURL = &clean
	}

	// Map legal entity name.
	if entityName, ok := v1Data["project_entity_name__c"].(string); ok && entityName != "" {
		payload.LegalEntityName = &entityName
	}

	// Map entity dissolution date.
	if dissolutionDate, ok := v1Data["project_entity_dissolution_date__c"].(string); ok && dissolutionDate != "" {
		// Extract just the date part from ISO 8601 datetime format.
		if dateOnly := strings.Split(dissolutionDate, "T")[0]; dateOnly != "" {
			payload.EntityDissolutionDate = &dateOnly
		}
	}

	// Map entity formation document URL.
	if formationDocURL, ok := v1Data["project_entity_formation_document__c"].(string); ok && isValidURL(formationDocURL) {
		clean := cleanURL(formationDocURL)
		payload.EntityFormationDocumentURL = &clean
	}

	// Map legal parent UID from parent_entity_relationship__c.
	if parentEntityID, ok := v1Data["parent_entity_relationship__c"].(string); ok && strings.TrimSpace(parentEntityID) != "" {
		parentEntityID = strings.TrimSpace(parentEntityID)
		// Look up the parent entity's V2 UID from SFID mappings.
		parentEntityMappingKey := fmt.Sprintf("project.sfid.%s", parentEntityID)
		if entry, err := mappingsKV.Get(ctx, parentEntityMappingKey); err == nil {
			legalParentUID := string(entry.Value())
			payload.LegalParentUID = &legalParentUID
			logger.With("parent_entity_sfid", parentEntityID, "legal_parent_uid", legalParentUID).DebugContext(ctx, "found legal parent UID from SFID mapping")
		} else {
			logger.With("parent_entity_sfid", parentEntityID, errKey, err).WarnContext(ctx, "could not find parent entity UID in mappings, leaving LegalParentUID empty")
		}
	}

	// Map settings fields.
	if missionStatement, ok := v1Data["mission_statement"].(string); ok && missionStatement != "" {
		payload.MissionStatement = &missionStatement
	}

	if announcementDate, ok := v1Data["expected_announcement_date__c"].(string); ok && announcementDate != "" {
		// Extract just the date part from ISO 8601 datetime format.
		if dateOnly := strings.Split(announcementDate, "T")[0]; dateOnly != "" {
			payload.AnnouncementDate = &dateOnly
		}
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
			payload.ParentUID = string(entry.Value())
			logger.With("parent_project_sfid", parentProjectID, "parent_uid", payload.ParentUID).DebugContext(ctx, "found parent project UID from SFID mapping")
		} else {
			logger.With("parent_project_sfid", parentProjectID, errKey, err).WarnContext(ctx, "could not find parent project UID in mappings, leaving ParentUID empty")
		}
	} else {
		// Project has no parent in V1, so it should be a child of ROOT in V2.
		rootUID, err := getRootProjectUID(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get ROOT project UID: %w", err)
		}
		payload.ParentUID = rootUID
		logger.With("root_uid", rootUID).DebugContext(ctx, "set project parent to ROOT")
	}

	// Map project stage from project_status__c only.
	if stage, ok := v1Data["project_status__c"].(string); ok && stage != "" {
		payload.Stage = &stage
	}

	return payload, nil
}

// mapV1DataToProjectUpdateBasePayload converts V1 project data to an UpdateProjectBasePayload.
func mapV1DataToProjectUpdateBasePayload(ctx context.Context, projectUID string, v1Data map[string]any, mappingsKV jetstream.KeyValue) (*projectservice.UpdateProjectBasePayload, error) {
	// Extract required fields.
	name, nameOK := v1Data["name"].(string)
	slug, slugOK := v1Data["slug__c"].(string)
	if !nameOK || !slugOK {
		return nil, fmt.Errorf("missing required fields: name=%t, slug=%t", nameOK, slugOK)
	}

	payload := &projectservice.UpdateProjectBasePayload{
		UID:  &projectUID,
		Name: name,
		Slug: strings.ToLower(slug),
	}

	// Map base fields.
	if desc, ok := v1Data["description__c"].(string); ok && desc != "" {
		payload.Description = desc
	}

	if public, ok := v1Data["public__c"].(bool); ok {
		payload.Public = &public
	}

	// Map category from admin_category__c with filtering.
	if adminCategory, ok := v1Data["admin_category__c"].(string); ok {
		payload.Category = mapAdminCategoryToCategory(adminCategory)
	}

	// Map legal entity type from category__c.
	if category, ok := v1Data["category__c"].(string); ok && category != "" {
		payload.LegalEntityType = &category
	}

	// Map funding model (split semicolon-delimited values).
	if model, ok := v1Data["model__c"].(string); ok && model != "" {
		models := strings.Split(model, ";")
		for i, m := range models {
			models[i] = strings.TrimSpace(m)
		}
		payload.FundingModel = models
	}

	// Map charter URL.
	if charterURL, ok := v1Data["charterurl__c"].(string); ok && isValidURL(charterURL) {
		clean := cleanURL(charterURL)
		payload.CharterURL = &clean
	}

	// Map autojoin enabled.
	if autoJoin, ok := v1Data["auto_join_enabled__c"].(bool); ok {
		payload.AutojoinEnabled = &autoJoin
	}

	// Map formation date (start_date__c).
	if startDate, ok := v1Data["start_date__c"].(string); ok && startDate != "" {
		// Extract just the date part from ISO 8601 datetime format.
		if dateOnly := strings.Split(startDate, "T")[0]; dateOnly != "" {
			payload.FormationDate = &dateOnly
		}
	}

	// Map project logo.
	if logo, ok := v1Data["project_logo__c"].(string); ok && isValidURL(logo) {
		clean := cleanURL(logo)
		payload.LogoURL = &clean
	}

	// Map repository URL.
	if repoURL, ok := v1Data["repositoryurl__c"].(string); ok && isValidURL(repoURL) {
		clean := cleanURL(repoURL)
		payload.RepositoryURL = &clean
	}

	// Map website URL.
	if websiteURL, ok := v1Data["website__c"].(string); ok && isValidURL(websiteURL) {
		clean := cleanURL(websiteURL)
		payload.WebsiteURL = &clean
	}

	// Map legal entity name.
	if entityName, ok := v1Data["project_entity_name__c"].(string); ok && entityName != "" {
		payload.LegalEntityName = &entityName
	}

	// Map entity dissolution date.
	if dissolutionDate, ok := v1Data["project_entity_dissolution_date__c"].(string); ok && dissolutionDate != "" {
		// Extract just the date part from ISO 8601 datetime format.
		if dateOnly := strings.Split(dissolutionDate, "T")[0]; dateOnly != "" {
			payload.EntityDissolutionDate = &dateOnly
		}
	}

	// Map entity formation document URL.
	if formationDocURL, ok := v1Data["project_entity_formation_document__c"].(string); ok && isValidURL(formationDocURL) {
		clean := cleanURL(formationDocURL)
		payload.EntityFormationDocumentURL = &clean
	}

	// Map legal parent UID from parent_entity_relationship__c.
	if parentEntityID, ok := v1Data["parent_entity_relationship__c"].(string); ok && strings.TrimSpace(parentEntityID) != "" {
		parentEntityID = strings.TrimSpace(parentEntityID)
		// Look up the parent entity's V2 UID from SFID mappings.
		parentEntityMappingKey := fmt.Sprintf("project.sfid.%s", parentEntityID)
		if entry, err := mappingsKV.Get(ctx, parentEntityMappingKey); err == nil {
			legalParentUID := string(entry.Value())
			payload.LegalParentUID = &legalParentUID
			logger.With("parent_entity_sfid", parentEntityID, "legal_parent_uid", legalParentUID).DebugContext(ctx, "found legal parent UID from SFID mapping")
		} else {
			logger.With("parent_entity_sfid", parentEntityID, errKey, err).WarnContext(ctx, "could not find parent entity UID in mappings, leaving LegalParentUID empty")
		}
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
			parentUID := string(entry.Value())
			payload.ParentUID = parentUID
			logger.With("parent_project_sfid", parentProjectID, "parent_uid", parentUID).DebugContext(ctx, "found parent project UID from SFID mapping")
		} else {
			logger.With("parent_project_sfid", parentProjectID, errKey, err).WarnContext(ctx, "could not find parent project UID in mappings, leaving ParentUID empty")
		}
	} else {
		// Project has no parent in V1, so it should be a child of ROOT in V2.
		rootUID, err := getRootProjectUID(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get ROOT project UID: %w", err)
		}
		payload.ParentUID = rootUID
		logger.With("root_uid", rootUID).DebugContext(ctx, "set project parent to ROOT")
	}

	// Map project stage from project_status__c only.
	if stage, ok := v1Data["project_status__c"].(string); ok && stage != "" {
		payload.Stage = &stage
	}

	return payload, nil
}

// mapV1DataToProjectUpdateSettingsPayload converts V1 project data to an UpdateProjectSettingsPayload.
func mapV1DataToProjectUpdateSettingsPayload(ctx context.Context, projectUID string, v1Data map[string]any) (*projectservice.UpdateProjectSettingsPayload, error) {
	payload := &projectservice.UpdateProjectSettingsPayload{
		UID: &projectUID,
	}

	// Map mission statement.
	if missionStatement, ok := v1Data["mission_statement"].(string); ok && missionStatement != "" {
		payload.MissionStatement = &missionStatement
	}

	// Map announcement date.
	if announcementDate, ok := v1Data["expected_announcement_date__c"].(string); ok && announcementDate != "" {
		// Extract just the date part from ISO 8601 datetime format.
		if dateOnly := strings.Split(announcementDate, "T")[0]; dateOnly != "" {
			payload.AnnouncementDate = &dateOnly
		}
	}

	return payload, nil
}
