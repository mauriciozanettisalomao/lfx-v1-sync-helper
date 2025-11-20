# LFX V1 Sync Helper

The LFX V1 Sync Helper is a Go microservice that synchronizes V1 data from NATS KV buckets to V2 services via authenticated API calls. It uses a JetStream consumer-based architecture that enables horizontal scaling across multiple service instances for processing V1 data updates.

## Architecture Overview

### Key Changes from Previous Version

1. **KV Bucket Watchers**: Instead of consuming NATS messages directly, the service now watches NATS KV buckets where Meltano writes V1 data
2. **Direct API Calls**: Replaced NATS message routing to indexer/fga-sync with direct authenticated API calls to Project and Committee services
3. **JWT Authentication**: Uses Heimdall-generated JWT tokens for secure API authentication
4. **Mapping Storage**: Maintains V1-to-V2 ID mappings in a dedicated NATS KV bucket

### Data Flow

```
Meltano → v1-objects KV Bucket → JetStream Consumer (Load Balanced) → API Calls → Project/Committee Services
```

1. **Meltano** writes V1 data (projects, committees) to the `v1-objects` NATS KV bucket
2. **JetStream Consumer** detects changes and distributes updates across multiple instances
3. **Service** makes authenticated API calls to V2 services
4. **Load Balancing** ensures each update is processed by exactly one instance
5. **Mappings** are stored in the `v1-mappings` KV bucket for future reference

## Horizontal Scaling

The service is designed to scale horizontally across multiple instances using a JetStream consumer with delivery groups:

### Scaling Architecture

- **JetStream Pull Consumer**: Replaces the previous KV Watch() method to enable load balancing
- **Competitive Consumption**: Multiple instances compete to fetch messages from the same consumer
- **Message Distribution**: Each KV bucket update is processed by exactly one instance
- **Load Balancing**: Instances automatically fetch messages when available, distributing work naturally
- **Fault Tolerance**: Failed messages are redelivered to other instances (max 3 attempts)

### Consumer Configuration

The service creates a JetStream pull consumer with the following properties:

```yaml
name: v1-sync-helper-kv-consumer
stream: KV_v1-objects
filterSubject: "$KV.v1-objects.>"
ackPolicy: explicit
maxDeliver: 3
ackWait: 30s
maxAckPending: 1000
```

### Scaling Operations

```bash
# Scale to 3 instances
helm upgrade lfx-v1-sync-helper ./charts/lfx-v1-sync-helper \
  --set replicas=3

# Scale down to 1 instance
helm upgrade lfx-v1-sync-helper ./charts/lfx-v1-sync-helper \
  --set replicas=1
```

### Benefits vs KV Watch()

| Feature | KV Watch() | JetStream Pull Consumer |
|---------|------------|------------------------|
| Scaling | All instances get same updates | Load balanced across instances |
| Reliability | No acknowledgments | Explicit acknowledgments with retries |
| Ordering | No guarantees | Configurable ordering per key |
| Monitoring | Limited | Rich metrics via NATS |
| Resource Usage | Push-based (constant connections) | Pull-based (fetch on demand) |

### Supported Objects

- **Projects**: Salesforce Project records
- **Committees**: Salesforce Collaboration records
- **Committee Members**: (planned for future implementation)

## Configuration

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `NATS_URL` | Yes | NATS server URL (e.g., `nats://localhost:4222`) |
| `PROJECT_SERVICE_URL` | Yes | Project Service API URL |
| `COMMITTEE_SERVICE_URL` | No | Committee Service API URL |
| `HEIMDALL_CLIENT_ID` | No | Client ID for JWT claims (default: `v1_sync_helper`) |
| `HEIMDALL_PRIVATE_KEY` | Yes | JWT private key (PEM format) for v2 services |
| `HEIMDALL_KEY_ID` | No | JWT key ID (if not provided, fetches from JWKS) |
| `HEIMDALL_JWKS_URL` | No | JWKS endpoint URL (default: cluster service) |
| `AUTH0_TENANT` | Yes | Auth0 tenant name (without .auth0.com suffix) |
| `AUTH0_CLIENT_ID` | Yes | Auth0 client ID for v1 API authentication |
| `AUTH0_PRIVATE_KEY` | Yes | Auth0 private key (PEM format) for v1 API |
| `LFX_API_GW` | No | LFX API Gateway URL (default: `https://api-gw.dev.platform.linuxfoundation.org/`) |
| `PORT` | No | HTTP server port (default: `8080`) |
| `BIND` | No | Interface to bind on (default: `*`) |
| `DEBUG` | No | Enable debug logging (default: `false`) |

### Heimdall Configuration

The service requires authentication configuration for both LFX v2 and v1 APIs:

```bash
# LFX v2 Heimdall Authentication
# Optional - used for principal and subject claims (default: v1_sync_helper)
export HEIMDALL_CLIENT_ID="v1_sync_helper"
export HEIMDALL_PRIVATE_KEY="$(kubectl get secret/heimdall-signer-cert -n lfx -o json | jq -r '.data["signer.pem"]' | base64 --decode)"
# Optional - if not provided, fetches from JWKS endpoint
export HEIMDALL_KEY_ID="your-key-id"
# Optional - defaults to cluster service
export HEIMDALL_JWKS_URL="http://lfx-platform-heimdall.lfx.svc.cluster.local:4457/.well-known/jwks"

# LFX v1 Auth0 Authentication
export AUTH0_TENANT="linuxfoundation-dev"
export AUTH0_CLIENT_ID="your-auth0-client-id"
export AUTH0_PRIVATE_KEY="$(cat auth0-private-key.pem)"
# Optional - defaults to dev environment
export LFX_API_GW="https://api-gw.dev.platform.linuxfoundation.org/"
```

JWT token configuration:
- **Algorithm**: PS256 (RSA-PSS with SHA-256)
- **Issuer**: `heimdall` (fixed)
- **Client ID**: Configurable via `HEIMDALL_CLIENT_ID` (default: `v1_sync_helper`)
- **Audiences**: Service-specific (hardcoded)
  - Project Service: `lfx-v2-project-service`
  - Committee Service: `lfx-v2-committee-service`
- **Key ID**: From config or JWKS endpoint

#### User Impersonation Logic

The service implements intelligent user impersonation based on V1 data with caching:

**Machine User Mode** (when `lastmodifiedbyid` has `@clients` suffix):
- **Principal**: `{client_id}@clients` (passed through verbatim)
- **Subject**: `{client_id}` (without @clients suffix)
- **Email**: Not included

**User Impersonation Mode** (when V1 platform ID is found):
- Looks up user via LFX v1 User Service API: `GET /v1/users/{platformID}`
- **Principal**: `{username}` (from API response)
- **Subject**: `{username}` (same as principal)
- **Email**: `{email}` (from API response if available)
- **Caching**: User data cached for 1 hour with background refresh
- **Locking**: Prevents concurrent API calls for same user

**Fallback Client Mode** (when no principal or lookup fails):
- **Principal**: `v1_sync_helper@clients`
- **Subject**: `v1_sync_helper`
- **Email**: Not included

This approach ensures that V1 sync operations are properly attributed to the actual user who made the changes, with efficient caching to minimize API calls.

## Development

### Prerequisites

- Go 1.24+
- NATS Server with JetStream enabled
- Access to Heimdall JWT configuration

### Building

```bash
# Build the binary
make build

# Build with debug symbols
make debug

# Run all checks (format, lint, test)
make check

# Run the service
make run
```

### Testing

```bash
# Run tests
make test

# Run tests with coverage
make test-coverage
```

### Docker

```bash
# Build Docker image
make docker-build

# Run in Docker
make docker-run
```

## Deployment

### Kubernetes with Helm

```bash
# Install the chart
helm install lfx-v1-sync-helper ./charts/lfx-v1-sync-helper \
  --set heimdall.secret.name=lfx-platform-heimdall-jwt-config \
  --namespace lfx

# Upgrade the chart
helm upgrade lfx-v1-sync-helper ./charts/lfx-v1-sync-helper \
  --namespace lfx
```

### Required NATS KV Buckets

The service requires two NATS KV buckets:

1. **`v1-objects`**: Stores V1 objects from Meltano (10GB capacity)
2. **`v1-mappings`**: Stores V1-to-V2 ID mappings (2GB capacity)

These are automatically created by the Helm chart.

## API Integration

### JWT Token Generation

The service generates JWT tokens for each API call using PS256 algorithm:

- **Algorithm**: PS256 (RSA-PSS with SHA-256)
- **Issuer**: `heimdall` (fixed)
- **Subject**: Configurable client ID (default: `v1_sync_helper`)
- **Principal**: `{client_id}@clients` (default: `v1_sync_helper@clients`)
- **Key ID**: From config or JWKS endpoint
- **Audiences**: Service-specific (hardcoded)
  - Project Service: `lfx-v2-project-service`
  - Committee Service: `lfx-v2-committee-service`
- **Expiration**: 5 minutes
- **Headers**: `Authorization: Bearer <token>` and optional `X-On-Behalf-Of: <username>`

Note: Each service has its own dedicated JWT audience - there is no "common" audience configuration.

### Project API Calls

- **Create**: `POST /projects` with project data
- **Update**: `PUT /projects/{uid}` with project data

### Committee API Calls

- **Create**: `POST /committees` with committee data
- **Update**: `PUT /committees/{uid}` with committee data

## Data Mapping

### Project Mapping

V1 Salesforce fields are mapped to V2 project fields:

| V1 Field | V2 Field |
|----------|----------|
| `name` | `name` |
| `slug__c` | `slug` |
| `description__c` | `description` |
| `public__c` | `public` |
| `formation_date__c` | `formation_date` |
| `legal_entity_name__c` | `legal_entity_name` |
| `legal_entity_type__c` | `legal_entity_type` |
| `logo_url__c` | `logo_url` |
| `repository_url__c` | `repository_url` |
| `stage__c` | `stage` |
| `website_url__c` | `website_url` |

### Committee Mapping

V1 Salesforce collaboration fields are mapped to V2 committee fields:

| V1 Field | V2 Field |
|----------|----------|
| `mailing_list__c` | `name` |
| `description__c` | `description` |
| `enable_voting__c` | `enable_voting` |
| `is_audit` | `is_audit` |
| `type__c` | `type` |
| `public_enabled` | `public` |
| `committee_website__c` | `website_url` |
| `sso_group_enabled` | `sso_group_enabled` |
| `sso_group_name` | `sso_group_name` |

## Monitoring

### Health Endpoints

- **`/livez`**: Liveness probe (always returns OK while service is running)
- **`/readyz`**: Readiness probe (checks NATS connection status)

### Logging

The service uses structured JSON logging with the following levels:

- **ERROR**: Critical errors that require attention
- **WARN**: Non-critical issues (e.g., user lookup failures)
- **INFO**: Important operations (e.g., successful project creation)
- **DEBUG**: Detailed operation information (enabled with `DEBUG=true`)

### Key Log Fields

- `key`: KV bucket key being processed
- `operation`: KV operation type (PUT, DELETE)
- `slug`/`sfid`: Object identifiers
- `project_uid`/`committee_uid`: Generated V2 UUIDs
- `username`: Extracted from V1 `lastmodifiedbyid`

## Legacy WAL-Listener Integration

The service includes commented-out WAL-listener handlers that can be enabled when needed. These handlers would:

1. Receive CDC events from the WAL-listener
2. Write data to the same `v1-objects` KV bucket as Meltano
3. Allow the KV watcher to process the updates uniformly

To enable WAL-listener integration, uncomment the relevant sections in `main.go`.

## Future Enhancements

1. **Committee Members**: Sync committee membership data
2. **Batch Processing**: Handle bulk updates more efficiently
3. **Retry Logic**: Add exponential backoff for failed API calls
4. **Metrics**: Add Prometheus metrics for monitoring
5. **Delete Operations**: Implement proper delete handling
6. **User Lookup**: Integrate with LFX User Service for proper username resolution

## Contributing

1. Follow the existing code style and patterns
2. Add tests for new functionality
3. Update documentation for any API changes
4. Ensure all checks pass with `make check`

## License

Copyright The Linux Foundation and each contributor to LFX.
SPDX-License-Identifier: MIT
