// JWT authentication and HTTP client for LFX v2 service calls with user impersonation
package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// Service audiences for JWT tokens
	projectServiceAudience   = "lfx-v2-project-service"
	committeeServiceAudience = "lfx-v2-committee-service"
)

var (
	httpClient     *http.Client
	jwtPrivateKey  *rsa.PrivateKey
	jwtKeyID       string
	jwtClientID    string
	heimdallConfig *Config
)

// JWKSResponse represents the JWKS endpoint response.
type JWKSResponse struct {
	Keys []JWK `json:"keys"`
}

// JWK represents a JSON Web Key.
type JWK struct {
	Kid string `json:"kid"`
}

// initJWTClient initializes the JWT authentication and HTTP client
func initJWTClient(cfg *Config) error {
	heimdallConfig = cfg
	// Parse the private key
	block, _ := pem.Decode([]byte(cfg.HeimdallPrivateKey))
	if block == nil {
		return fmt.Errorf("failed to parse PEM block containing the private key")
	}

	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8 format if PKCS1 fails
		privateKeyInterface, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return fmt.Errorf("failed to parse private key: %w", err)
		}
		var ok bool
		privateKey, ok = privateKeyInterface.(*rsa.PrivateKey)
		if !ok {
			return fmt.Errorf("private key is not RSA")
		}
	}

	jwtPrivateKey = privateKey
	jwtClientID = cfg.HeimdallClientID

	// Get or fetch the key ID
	jwtKeyID, err = getKeyID(cfg)
	if err != nil {
		return fmt.Errorf("failed to get key ID: %w", err)
	}

	// Create HTTP client with timeout
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	return nil
}

// getKeyID gets the JWT key ID from config or fetches it from JWKS endpoint.
func getKeyID(cfg *Config) (string, error) {
	// Use config value if provided
	if cfg.HeimdallKeyID != "" {
		return cfg.HeimdallKeyID, nil
	}

	// Fetch from JWKS endpoint
	resp, err := http.Get(cfg.HeimdallJWKSURL)
	if err != nil {
		return "", fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("JWKS endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read JWKS response: %w", err)
	}

	var jwks JWKSResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return "", fmt.Errorf("failed to parse JWKS response: %w", err)
	}

	if len(jwks.Keys) == 0 {
		return "", fmt.Errorf("no keys found in JWKS response")
	}

	if jwks.Keys[0].Kid == "" {
		return "", fmt.Errorf("no key ID found in first JWKS key")
	}

	return jwks.Keys[0].Kid, nil
}

// generateJWTToken generates a JWT token for API authentication with optional user impersonation.
//
// This function implements a dual authentication strategy:
//  1. User Impersonation: If userInfo contains a username, the JWT will impersonate that user
//     with principal=username, sub=username, and email (if available)
//  2. Fallback Client: If no user info is provided, uses v1_sync_helper client credentials
//     with principal="v1_sync_helper@clients" and sub="v1_sync_helper"
//
// The impersonation approach allows V1 sync operations to be attributed to the actual
// user who made the changes in V1, rather than a generic service account.
func generateJWTToken(audience string, userInfo UserInfo) (string, error) {
	now := time.Now()

	var principal, subject string

	// If we have user info with a username, impersonate that user
	if userInfo.Username != "" {
		// Use Principal if set (for machine users with @clients), otherwise use Username
		if userInfo.Principal != "" {
			principal = userInfo.Principal // Machine user with @clients suffix
		} else {
			principal = userInfo.Username // Regular user
		}
		subject = userInfo.Username // Subject is always without @clients suffix
		logger.With("username", userInfo.Username, "principal", principal, "email", userInfo.Email, "audience", audience).Debug("generating JWT token with user impersonation")
	} else {
		// Fallback to v1_sync_helper client ID
		principal = jwtClientID + "@clients"
		subject = jwtClientID
		logger.With("client_id", jwtClientID, "audience", audience).Debug("generating JWT token with fallback client authentication")
	}

	claims := jwt.MapClaims{
		"iss":       "heimdall",
		"sub":       subject,
		"aud":       audience,
		"iat":       now.Unix(),
		"exp":       now.Add(5 * time.Minute).Unix(), // Token expires in 5 minutes
		"nbf":       now.Unix(),                      // Not before (valid from now)
		"jti":       uuid.New().String(),             // Unique JWT ID
		"principal": principal,
	}

	// Add email if available and we're impersonating a user
	if userInfo.Username != "" && userInfo.Email != "" {
		claims["email"] = userInfo.Email
	}

	// Create token with PS256 algorithm and kid header
	token := jwt.NewWithClaims(jwt.SigningMethodPS256, claims)
	token.Header["kid"] = jwtKeyID

	return token.SignedString(jwtPrivateKey)
}

// ProjectRequest represents a project creation/update request
type ProjectRequest struct {
	UID             string    `json:"uid,omitempty"`
	Name            string    `json:"name"`
	Slug            string    `json:"slug"`
	Description     string    `json:"description"`
	Public          bool      `json:"public"`
	ParentUID       string    `json:"parent_uid,omitempty"`
	FormationDate   string    `json:"formation_date,omitempty"`
	LegalEntityName string    `json:"legal_entity_name,omitempty"`
	LegalEntityType string    `json:"legal_entity_type,omitempty"`
	LogoURL         string    `json:"logo_url,omitempty"`
	RepositoryURL   string    `json:"repository_url,omitempty"`
	Stage           string    `json:"stage,omitempty"`
	WebsiteURL      string    `json:"website_url,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// ProjectResponse represents a project API response
type ProjectResponse struct {
	UID             string    `json:"uid"`
	Name            string    `json:"name"`
	Slug            string    `json:"slug"`
	Description     string    `json:"description"`
	Public          bool      `json:"public"`
	ParentUID       string    `json:"parent_uid,omitempty"`
	FormationDate   string    `json:"formation_date,omitempty"`
	LegalEntityName string    `json:"legal_entity_name,omitempty"`
	LegalEntityType string    `json:"legal_entity_type,omitempty"`
	LogoURL         string    `json:"logo_url,omitempty"`
	RepositoryURL   string    `json:"repository_url,omitempty"`
	Stage           string    `json:"stage,omitempty"`
	WebsiteURL      string    `json:"website_url,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// CommitteeRequest represents a committee creation/update request
type CommitteeRequest struct {
	UID             string    `json:"uid,omitempty"`
	Name            string    `json:"name"`
	ProjectUID      string    `json:"project_uid"`
	Description     string    `json:"description"`
	EnableVoting    bool      `json:"enable_voting"`
	IsAudit         bool      `json:"is_audit"`
	Type            string    `json:"type"`
	Public          bool      `json:"public"`
	PublicName      string    `json:"public_name,omitempty"`
	CommitteeID     string    `json:"committee_id,omitempty"`
	WebsiteURL      string    `json:"website_url,omitempty"`
	SSOGroupEnabled bool      `json:"sso_group_enabled"`
	SSOGroupName    string    `json:"sso_group_name,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// CommitteeResponse represents a committee API response
type CommitteeResponse struct {
	UID             string    `json:"uid"`
	Name            string    `json:"name"`
	ProjectUID      string    `json:"project_uid"`
	Description     string    `json:"description"`
	EnableVoting    bool      `json:"enable_voting"`
	IsAudit         bool      `json:"is_audit"`
	Type            string    `json:"type"`
	Public          bool      `json:"public"`
	PublicName      string    `json:"public_name,omitempty"`
	CommitteeID     string    `json:"committee_id,omitempty"`
	WebsiteURL      string    `json:"website_url,omitempty"`
	SSOGroupEnabled bool      `json:"sso_group_enabled"`
	SSOGroupName    string    `json:"sso_group_name,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// fetchProject fetches an existing project from the Project Service API
func fetchProject(ctx context.Context, projectUID string, userInfo UserInfo) (*ProjectResponse, string, error) {
	url := fmt.Sprintf("%s/projects/%s", cfg.ProjectServiceURL.String(), projectUID)

	// Generate JWT token
	token, err := generateJWTToken(projectServiceAudience, userInfo)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate JWT token: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	if userInfo.Username != "" {
		req.Header.Set("X-On-Behalf-Of", userInfo.Username)
	}

	// Send the request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Get ETag from response headers
	etag := resp.Header.Get("ETag")

	// Parse response
	var projectResponse ProjectResponse
	if err := json.Unmarshal(respBody, &projectResponse); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal project response: %w", err)
	}

	return &projectResponse, etag, nil
}

// createProject creates a new project via the Project Service API with user impersonation
func createProject(ctx context.Context, project ProjectRequest, userInfo UserInfo) (*ProjectResponse, error) {
	url := fmt.Sprintf("%s/projects", cfg.ProjectServiceURL.String())
	return sendProjectRequest(ctx, "POST", url, project, userInfo)
}

// updateProject updates an existing project via the Project Service API with user impersonation using fetch-merge-update pattern
func updateProject(ctx context.Context, projectUID string, project ProjectRequest, userInfo UserInfo) (*ProjectResponse, error) {
	// Fetch the existing project to get current state and ETag
	existing, etag, err := fetchProject(ctx, projectUID, userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch existing project: %w", err)
	}

	// Merge V1 mapped attributes into the existing V2 object
	merged := *existing
	if project.Name != "" {
		merged.Name = project.Name
	}
	if project.Slug != "" {
		merged.Slug = project.Slug
	}
	if project.Description != "" {
		merged.Description = project.Description
	}
	merged.Public = project.Public
	if project.ParentUID != "" {
		merged.ParentUID = project.ParentUID
	}
	if project.FormationDate != "" {
		merged.FormationDate = project.FormationDate
	}
	if project.LegalEntityName != "" {
		merged.LegalEntityName = project.LegalEntityName
	}
	if project.LegalEntityType != "" {
		merged.LegalEntityType = project.LegalEntityType
	}
	if project.LogoURL != "" {
		merged.LogoURL = project.LogoURL
	}
	if project.RepositoryURL != "" {
		merged.RepositoryURL = project.RepositoryURL
	}
	if project.Stage != "" {
		merged.Stage = project.Stage
	}
	if project.WebsiteURL != "" {
		merged.WebsiteURL = project.WebsiteURL
	}
	if !project.CreatedAt.IsZero() {
		merged.CreatedAt = project.CreatedAt
	}
	if !project.UpdatedAt.IsZero() {
		merged.UpdatedAt = project.UpdatedAt
	}

	// Convert response back to request format
	mergedRequest := ProjectRequest{
		UID:             merged.UID,
		Name:            merged.Name,
		Slug:            merged.Slug,
		Description:     merged.Description,
		Public:          merged.Public,
		ParentUID:       merged.ParentUID,
		FormationDate:   merged.FormationDate,
		LegalEntityName: merged.LegalEntityName,
		LegalEntityType: merged.LegalEntityType,
		LogoURL:         merged.LogoURL,
		RepositoryURL:   merged.RepositoryURL,
		Stage:           merged.Stage,
		WebsiteURL:      merged.WebsiteURL,
		CreatedAt:       merged.CreatedAt,
		UpdatedAt:       merged.UpdatedAt,
	}

	// Send PUT request with If-Match header
	url := fmt.Sprintf("%s/projects/%s", cfg.ProjectServiceURL.String(), projectUID)
	return sendProjectRequestETag(ctx, "PUT", url, mergedRequest, userInfo, etag)
}

// sendProjectRequestETag sends an HTTP request to the Project Service with user impersonation and ETag
func sendProjectRequestETag(ctx context.Context, method, url string, project ProjectRequest, userInfo UserInfo, etag string) (*ProjectResponse, error) {
	// Generate JWT token for the Project Service audience with user impersonation if available
	token, err := generateJWTToken(projectServiceAudience, userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to generate JWT token: %w", err)
	}

	// Marshal the request body
	body, err := json.Marshal(project)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal project request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	if userInfo.Username != "" {
		req.Header.Set("X-On-Behalf-Of", userInfo.Username)
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}

	// Send the request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var projectResponse ProjectResponse
	if err := json.Unmarshal(respBody, &projectResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal project response: %w", err)
	}

	return &projectResponse, nil
}

// sendProjectRequest sends an HTTP request to the Project Service with user impersonation
func sendProjectRequest(ctx context.Context, method, url string, project ProjectRequest, userInfo UserInfo) (*ProjectResponse, error) {
	return sendProjectRequestETag(ctx, method, url, project, userInfo, "")
}

// fetchCommittee fetches an existing committee from the Committee Service API
func fetchCommittee(ctx context.Context, committeeUID string, userInfo UserInfo) (*CommitteeResponse, string, error) {
	if cfg.CommitteeServiceURL == nil {
		return nil, "", fmt.Errorf("committee service URL not configured")
	}
	url := fmt.Sprintf("%s/committees/%s", cfg.CommitteeServiceURL.String(), committeeUID)

	// Generate JWT token
	token, err := generateJWTToken(committeeServiceAudience, userInfo)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate JWT token: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	if userInfo.Username != "" {
		req.Header.Set("X-On-Behalf-Of", userInfo.Username)
	}

	// Send the request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Get ETag from response headers
	etag := resp.Header.Get("ETag")

	// Parse response
	var committeeResponse CommitteeResponse
	if err := json.Unmarshal(respBody, &committeeResponse); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal committee response: %w", err)
	}

	return &committeeResponse, etag, nil
}

// createCommittee creates a new committee via the Committee Service API with user impersonation
func createCommittee(ctx context.Context, committee CommitteeRequest, userInfo UserInfo) (*CommitteeResponse, error) {
	if cfg.CommitteeServiceURL == nil {
		return nil, fmt.Errorf("committee service URL not configured")
	}
	url := fmt.Sprintf("%s/committees", cfg.CommitteeServiceURL.String())
	return sendCommitteeRequest(ctx, "POST", url, committee, userInfo)
}

// updateCommittee updates an existing committee via the Committee Service API with user impersonation using fetch-merge-update pattern
func updateCommittee(ctx context.Context, committeeUID string, committee CommitteeRequest, userInfo UserInfo) (*CommitteeResponse, error) {
	// Fetch the existing committee to get current state and ETag
	existing, etag, err := fetchCommittee(ctx, committeeUID, userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch existing committee: %w", err)
	}

	// Merge V1 mapped attributes into the existing V2 object
	merged := *existing
	if committee.Name != "" {
		merged.Name = committee.Name
	}
	if committee.ProjectUID != "" {
		merged.ProjectUID = committee.ProjectUID
	}
	if committee.Description != "" {
		merged.Description = committee.Description
	}
	merged.EnableVoting = committee.EnableVoting
	merged.IsAudit = committee.IsAudit
	if committee.Type != "" {
		merged.Type = committee.Type
	}
	merged.Public = committee.Public
	if committee.PublicName != "" {
		merged.PublicName = committee.PublicName
	}
	if committee.CommitteeID != "" {
		merged.CommitteeID = committee.CommitteeID
	}
	if committee.WebsiteURL != "" {
		merged.WebsiteURL = committee.WebsiteURL
	}
	merged.SSOGroupEnabled = committee.SSOGroupEnabled
	if committee.SSOGroupName != "" {
		merged.SSOGroupName = committee.SSOGroupName
	}
	if !committee.CreatedAt.IsZero() {
		merged.CreatedAt = committee.CreatedAt
	}
	if !committee.UpdatedAt.IsZero() {
		merged.UpdatedAt = committee.UpdatedAt
	}

	// Convert response back to request format
	mergedRequest := CommitteeRequest{
		UID:             merged.UID,
		Name:            merged.Name,
		ProjectUID:      merged.ProjectUID,
		Description:     merged.Description,
		EnableVoting:    merged.EnableVoting,
		IsAudit:         merged.IsAudit,
		Type:            merged.Type,
		Public:          merged.Public,
		PublicName:      merged.PublicName,
		CommitteeID:     merged.CommitteeID,
		WebsiteURL:      merged.WebsiteURL,
		SSOGroupEnabled: merged.SSOGroupEnabled,
		SSOGroupName:    merged.SSOGroupName,
		CreatedAt:       merged.CreatedAt,
		UpdatedAt:       merged.UpdatedAt,
	}

	// Send PUT request with If-Match header
	if cfg.CommitteeServiceURL == nil {
		return nil, fmt.Errorf("committee service URL not configured")
	}
	url := fmt.Sprintf("%s/committees/%s", cfg.CommitteeServiceURL.String(), committeeUID)
	return sendCommitteeRequestETag(ctx, "PUT", url, mergedRequest, userInfo, etag)
}

// sendCommitteeRequestETag sends an HTTP request to the Committee Service with user impersonation and ETag
func sendCommitteeRequestETag(ctx context.Context, method, url string, committee CommitteeRequest, userInfo UserInfo, etag string) (*CommitteeResponse, error) {
	// Generate JWT token for the Committee Service audience with user impersonation if available
	token, err := generateJWTToken(committeeServiceAudience, userInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to generate JWT token: %w", err)
	}

	// Marshal the request body
	body, err := json.Marshal(committee)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal committee request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	if userInfo.Username != "" {
		req.Header.Set("X-On-Behalf-Of", userInfo.Username)
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}

	// Send the request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for HTTP errors
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var committeeResponse CommitteeResponse
	if err := json.Unmarshal(respBody, &committeeResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal committee response: %w", err)
	}

	return &committeeResponse, nil
}

// sendCommitteeRequest sends an HTTP request to the Committee Service with user impersonation
func sendCommitteeRequest(ctx context.Context, method, url string, committee CommitteeRequest, userInfo UserInfo) (*CommitteeResponse, error) {
	return sendCommitteeRequestETag(ctx, method, url, committee, userInfo, "")
}

// UserInfo represents the user information we need
type UserInfo struct {
	Username  string
	Email     string
	Principal string // Optional: if set, used as principal instead of username
}
