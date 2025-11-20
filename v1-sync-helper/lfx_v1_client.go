// Auth0 authentication and HTTP client for LFX v1 API Gateway calls
//
// This client handles:
// 1. Auth0 private key JWT authentication for LFX v1 API access
// 2. User lookup via v1 User Service API with intelligent caching
// 3. Machine user detection (platform IDs ending with "@clients")
// 4. Concurrent request locking to prevent duplicate API calls
// 5. Background cache refresh for stale entries
//
// Caching Strategy:
// - User data cached for 1 hour in NATS KV store
// - Stale cache entries trigger background refresh
// - Lock mechanism prevents concurrent API calls for same user
// - Error states are cached to avoid repeated failed lookups
//
// User Types:
// - Machine users: platform IDs with "@clients" suffix (no API lookup)
// - Platform users: regular platform IDs requiring v1 API lookup
// - Invalid users: empty usernames or API errors (cached to prevent retries)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/auth0/go-auth0/authentication"
	"github.com/auth0/go-auth0/authentication/oauth"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/oauth2"
)

const (
	// Cache settings for user lookups
	userCacheKeyPrefix    = "v1_user."
	userLockKeyPrefix     = "v1_user_lock."
	userCacheExpiry       = 1 * time.Hour    // Cache user data for 1 hour
	userLockTimeout       = 10 * time.Second // Lock timeout for concurrent requests
	userLockRetryInterval = 1 * time.Second  // Retry interval when lock exists
)

var (
	v1HTTPClient *http.Client
	auth0Config  *authentication.Authentication
)

// V1User represents a user from the LFX v1 User Service
type V1User struct {
	ID          string    `json:"ID"`
	Username    string    `json:"Username"`
	Email       string    `json:"Email"`
	FirstName   string    `json:"FirstName"`
	LastName    string    `json:"LastName"`
	LastFetched time.Time `json:"_last_fetched"` // Internal field for cache management
}

// V1UserResponse represents the API response from v1 User Service
type V1UserResponse struct {
	ID        string `json:"ID"`
	Username  string `json:"Username"`
	Email     string `json:"Email"`
	FirstName string `json:"FirstName"`
	LastName  string `json:"LastName"`
}

// ClientCredentialsTokenSource implements oauth2.TokenSource for Auth0 private key JWT
type ClientCredentialsTokenSource struct {
	ctx        context.Context
	authConfig *authentication.Authentication
	audience   string
}

// Token implements the oauth2.TokenSource interface to return a new access token
func (c *ClientCredentialsTokenSource) Token() (*oauth2.Token, error) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.TODO()
	}

	// Build and issue a request using Auth0 client credentials flow
	body := oauth.LoginWithClientCredentialsRequest{
		Audience: c.audience,
	}

	tokenSet, err := c.authConfig.OAuth.LoginWithClientCredentials(ctx, body, oauth.IDTokenValidationOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Auth0 token: %w", err)
	}

	// Convert the Auth0 response to an oauth2.Token with leeway for clock skew
	const leeway = 60 * time.Second
	token := &oauth2.Token{
		AccessToken: tokenSet.AccessToken,
		TokenType:   tokenSet.TokenType,
		Expiry:      time.Now().Add(time.Duration(tokenSet.ExpiresIn)*time.Second - leeway),
	}

	return token, nil
}

// initV1Client initializes the Auth0 authentication and HTTP client for v1 API calls
func initV1Client(cfg *Config) error {
	// Create Auth0 client configuration with private key JWT
	authConfig, err := authentication.New(
		context.Background(),
		fmt.Sprintf("%s.auth0.com", cfg.Auth0Tenant),
		authentication.WithClientID(cfg.Auth0ClientID),
		authentication.WithClientAssertion(cfg.Auth0PrivateKey, "RS256"),
	)
	if err != nil {
		return fmt.Errorf("failed to create Auth0 client configuration: %w", err)
	}

	auth0Config = authConfig

	// Create HTTP client with Auth0 token source
	tokenSource := &ClientCredentialsTokenSource{
		ctx:        context.Background(),
		authConfig: authConfig,
		audience:   cfg.LFXAPIGateway.String(),
	}

	v1HTTPClient = oauth2.NewClient(nil, tokenSource)

	return nil
}

// getUserFromV1API fetches user information from the LFX v1 User Service
func getUserFromV1API(ctx context.Context, platformID string) (*V1User, error) {
	url := fmt.Sprintf("%sv1/users/%s", cfg.LFXAPIGateway.String(), platformID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := v1HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v1 User Service returned status %d: %s", resp.StatusCode, string(body))
	}

	var userResponse V1UserResponse
	if err := json.Unmarshal(body, &userResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user response: %w", err)
	}

	// Convert to internal user format with cache timestamp
	user := &V1User{
		ID:          userResponse.ID,
		Username:    userResponse.Username,
		Email:       userResponse.Email,
		FirstName:   userResponse.FirstName,
		LastName:    userResponse.LastName,
		LastFetched: time.Now().UTC(),
	}

	return user, nil
}

// getCachedUser retrieves a user from the mappings KV cache
func getCachedUser(ctx context.Context, platformID string, mappingsKV jetstream.KeyValue) (*V1User, error) {
	cacheKey := userCacheKeyPrefix + platformID

	entry, err := mappingsKV.Get(ctx, cacheKey)
	if err != nil {
		return nil, err // No cached entry
	}

	var user V1User
	if err := json.Unmarshal(entry.Value(), &user); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cached user: %w", err)
	}

	return &user, nil
}

// setCachedUser stores a user in the mappings KV cache
func setCachedUser(ctx context.Context, platformID string, user *V1User, mappingsKV jetstream.KeyValue) error {
	cacheKey := userCacheKeyPrefix + platformID

	data, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("failed to marshal user for cache: %w", err)
	}

	_, err = mappingsKV.Put(ctx, cacheKey, data)
	return err
}

// acquireUserLock attempts to acquire a lock for user refresh operations
func acquireUserLock(ctx context.Context, platformID string, mappingsKV jetstream.KeyValue) (bool, error) {
	lockKey := userLockKeyPrefix + platformID
	lockValue := strconv.FormatInt(time.Now().Unix(), 10)

	// Try to create the lock (will fail if it already exists)
	_, err := mappingsKV.Create(ctx, lockKey, []byte(lockValue))
	if err != nil {
		// Check if lock already exists and if it's stale
		if entry, getErr := mappingsKV.Get(ctx, lockKey); getErr == nil {
			if lockTimestamp, parseErr := strconv.ParseInt(string(entry.Value()), 10, 64); parseErr == nil {
				lockTime := time.Unix(lockTimestamp, 0)
				if time.Since(lockTime) > userLockTimeout {
					// Lock is stale, try to update it
					if _, updateErr := mappingsKV.Put(ctx, lockKey, []byte(lockValue)); updateErr == nil {
						return true, nil
					}
				}
			}
		}
		return false, nil // Lock exists and is not stale
	}

	return true, nil // Successfully acquired lock
}

// releaseUserLock releases a user refresh lock
func releaseUserLock(ctx context.Context, platformID string, mappingsKV jetstream.KeyValue) error {
	lockKey := userLockKeyPrefix + platformID
	return mappingsKV.Delete(ctx, lockKey)
}

// refreshUserInBackground refreshes user data in the background
func refreshUserInBackground(ctx context.Context, platformID string, mappingsKV jetstream.KeyValue) {
	go func() {
		// Acquire lock for this refresh operation
		acquired, err := acquireUserLock(ctx, platformID, mappingsKV)
		if err != nil || !acquired {
			return // Another process is already refreshing or error occurred
		}

		defer func() {
			if releaseErr := releaseUserLock(ctx, platformID, mappingsKV); releaseErr != nil {
				logger.With(errKey, releaseErr, "platform_id", platformID).WarnContext(ctx, "failed to release user cache lock")
			}
		}()

		// Fetch fresh user data
		user, err := getUserFromV1API(ctx, platformID)
		if err != nil {
			logger.With(errKey, err, "platform_id", platformID).WarnContext(ctx, "background user refresh failed")
			return
		}

		// Update cache
		if err := setCachedUser(ctx, platformID, user, mappingsKV); err != nil {
			logger.With(errKey, err, "platform_id", platformID).WarnContext(ctx, "failed to update user cache after refresh")
		} else {
			logger.With("platform_id", platformID, "username", user.Username).DebugContext(ctx, "user cache refreshed in background")
		}
	}()
}

// lookupUser retrieves user information with caching and refresh logic
func lookupUser(ctx context.Context, platformID string, mappingsKV jetstream.KeyValue) (*V1User, error) {
	// Check if this is already a machine user (contains @clients suffix)
	if strings.HasSuffix(platformID, "@clients") {
		// Return a synthetic user for machine accounts
		return &V1User{
			ID:          platformID,
			Username:    strings.TrimSuffix(platformID, "@clients"), // Remove @clients for username
			Email:       "",                                         // No email for machine users
			LastFetched: time.Now().UTC(),
		}, nil
	}

	// Try to get from cache first
	cachedUser, err := getCachedUser(ctx, platformID, mappingsKV)
	if err == nil {
		// Check if cache is stale
		if time.Since(cachedUser.LastFetched) > userCacheExpiry {
			// Cache is stale, refresh in background and return stale data
			refreshUserInBackground(ctx, platformID, mappingsKV)
		}
		return cachedUser, nil
	}

	// Not in cache, need to fetch fresh data
	// Try to acquire lock for this fetch operation
	acquired, err := acquireUserLock(ctx, platformID, mappingsKV)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire user lookup lock: %w", err)
	}

	if !acquired {
		// Another process is fetching, wait and check cache again
		time.Sleep(userLockRetryInterval)
		cachedUser, err := getCachedUser(ctx, platformID, mappingsKV)
		if err == nil {
			return cachedUser, nil
		}
		return nil, fmt.Errorf("user lookup timeout: concurrent fetch in progress")
	}

	defer func() {
		if releaseErr := releaseUserLock(ctx, platformID, mappingsKV); releaseErr != nil {
			logger.With(errKey, releaseErr, "platform_id", platformID).WarnContext(ctx, "failed to release user lookup lock")
		}
	}()

	// Double-check cache after acquiring lock
	cachedUser, err = getCachedUser(ctx, platformID, mappingsKV)
	if err == nil {
		return cachedUser, nil
	}

	// Fetch from API
	user, err := getUserFromV1API(ctx, platformID)
	if err != nil {
		// Cache the error state to avoid repeated failed lookups
		errorUser := &V1User{
			ID:          platformID,
			Username:    "", // Empty username indicates error state
			Email:       "",
			LastFetched: time.Now().UTC(),
		}
		if cacheErr := setCachedUser(ctx, platformID, errorUser, mappingsKV); cacheErr != nil {
			logger.With(errKey, cacheErr, "platform_id", platformID).WarnContext(ctx, "failed to cache error state for user")
		}
		return nil, err
	}

	// Validate required fields
	if user.Username == "" {
		logger.With("platform_id", platformID).WarnContext(ctx, "v1 user has empty username")
		// Cache the invalid state
		invalidUser := &V1User{
			ID:          platformID,
			Username:    "", // Empty username indicates invalid state
			Email:       "",
			LastFetched: time.Now().UTC(),
		}
		if cacheErr := setCachedUser(ctx, platformID, invalidUser, mappingsKV); cacheErr != nil {
			logger.With(errKey, cacheErr, "platform_id", platformID).WarnContext(ctx, "failed to cache invalid state for user")
		}
		return nil, fmt.Errorf("user has empty username")
	}

	// Cache the valid user
	if err := setCachedUser(ctx, platformID, user, mappingsKV); err != nil {
		logger.With(errKey, err, "platform_id", platformID).WarnContext(ctx, "failed to cache user data")
		// Continue anyway with the fetched user
	}

	return user, nil
}

// getUserInfoFromV1 converts a Platform ID to LFX username and email using v1 API with caching
func getUserInfoFromV1(ctx context.Context, platformID string, mappingsKV jetstream.KeyValue) (UserInfo, error) {
	if platformID == "" {
		return UserInfo{}, nil
	}

	user, err := lookupUser(ctx, platformID, mappingsKV)
	if err != nil {
		logger.With(errKey, err, "platform_id", platformID).WarnContext(ctx, "failed to lookup user from v1 API, falling back to service account")
		return UserInfo{}, nil // Return empty to trigger fallback
	}

	// Check for cached error/invalid states
	if user.Username == "" {
		logger.With("platform_id", platformID).WarnContext(ctx, "user has empty username, falling back to service account")
		return UserInfo{}, nil // Return empty to trigger fallback
	}

	// Return user info for JWT impersonation
	userInfo := UserInfo{
		Username: user.Username,
		Email:    user.Email,
	}

	return userInfo, nil
}
