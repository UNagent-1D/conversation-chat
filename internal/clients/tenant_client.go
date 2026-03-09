package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/UNagent-1D/conversation-chat/internal/domain"
)

// TenantDetail is the response from GET /api/v1/tenants/:id.
type TenantDetail struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Slug   string `json:"slug"`
	Plan   string `json:"plan"`
	Status string `json:"status"`
}

// ProfileDetail is a single profile from GET /api/v1/tenants/:id/profiles.
type ProfileDetail struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	AllowedSpecialties []string `json:"allowed_specialties"`
	AllowedLocations   []string `json:"allowed_locations"`
	AgentConfigID      *string  `json:"agent_config_id"`
}

// EndUserLookup is the response from GET /api/v1/tenants/:id/end-users/lookup/phone/:number.
type EndUserLookup struct {
	Exists  bool            `json:"exists"`
	Patient *PatientDetails `json:"patient,omitempty"`
}

// PatientDetails holds the resolved patient data.
type PatientDetails struct {
	ID          string `json:"id"`
	FullName    string `json:"full_name"`
	Cellphone   string `json:"cellphone"`
	ExternalRef string `json:"external_ref"`
}

// DataSource is a single data source record from the Tenant Service.
// NOTE: GET /api/v1/tenants/:id/data-sources is not yet in the Tenant Service spec.
// This struct and the GetDataSources method use a stub until that endpoint is added.
type DataSource struct {
	ID           string                    `json:"id"`
	Name         string                    `json:"name"`
	SourceType   string                    `json:"source_type"`
	BaseURL      string                    `json:"base_url"`
	RouteConfigs map[string]RouteConfigDTO `json:"route_configs"`
	IsActive     bool                      `json:"is_active"`
}

// RouteConfigDTO matches the Tenant Service route_configs shape.
type RouteConfigDTO struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// TenantClient calls the Tenant Service for tenant metadata, profiles, end-user lookup, and data sources.
type TenantClient struct {
	baseURL    string
	httpClient *http.Client
	token      string
}

// NewTenantClient creates a new TenantClient.
func NewTenantClient(baseURL, token string) *TenantClient {
	return &TenantClient{
		baseURL:    baseURL,
		httpClient: &http.Client{},
		token:      token,
	}
}

// GetTenant fetches tenant metadata (plan, status).
// Calls: GET /api/v1/tenants/:id
func (c *TenantClient) GetTenant(ctx context.Context, tenantID string) (*TenantDetail, error) {
	url := fmt.Sprintf("%s/api/v1/tenants/%s", c.baseURL, tenantID)
	return doGet[TenantDetail](ctx, c.httpClient, url, c.token)
}

// GetProfile fetches a specific profile by filtering the profile list.
// Calls: GET /api/v1/tenants/:id/profiles
func (c *TenantClient) GetProfile(ctx context.Context, tenantID, profileID string) (*ProfileDetail, error) {
	endpoint := fmt.Sprintf("%s/api/v1/tenants/%s/profiles", c.baseURL, tenantID)

	type profileList struct {
		Data []ProfileDetail `json:"data"`
	}
	list, err := doGet[profileList](ctx, c.httpClient, endpoint, c.token)
	if err != nil {
		return nil, err
	}
	for _, p := range list.Data {
		if p.ID == profileID {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("profile %s not found for tenant %s", profileID, tenantID)
}

// LookupByPhone resolves an end-user from their E.164 phone number.
// Calls: GET /api/v1/tenants/:id/end-users/lookup/phone/:number
func (c *TenantClient) LookupByPhone(ctx context.Context, tenantID, phone string) (*EndUserLookup, error) {
	encoded := url.PathEscape(phone)
	endpoint := fmt.Sprintf("%s/api/v1/tenants/%s/end-users/lookup/phone/%s", c.baseURL, tenantID, encoded)
	return doGet[EndUserLookup](ctx, c.httpClient, endpoint, c.token)
}

// GetDataSources retrieves all active data sources for a tenant.
// Calls: GET /api/v1/tenants/:id/data-sources
// Only active records are returned (filtered server-side). credential_ref is never exposed.
func (c *TenantClient) GetDataSources(ctx context.Context, tenantID string) ([]*DataSource, error) {
	endpoint := fmt.Sprintf("%s/api/v1/tenants/%s/data-sources", c.baseURL, tenantID)

	type dsList struct {
		Data []*DataSource `json:"data"`
	}
	list, err := doGet[dsList](ctx, c.httpClient, endpoint, c.token)
	if err != nil {
		return nil, fmt.Errorf("get data sources: %w", err)
	}
	return list.Data, nil
}

// BuildRouteConfigs flattens data sources into the domain RouteConfig map keyed by tool_name.
// base_url is pulled from the DataSource, path from the RouteConfigDTO.
func BuildRouteConfigs(sources []*DataSource) map[string]domain.RouteConfig {
	result := make(map[string]domain.RouteConfig)
	for _, ds := range sources {
		if !ds.IsActive {
			continue
		}
		for toolName, rc := range ds.RouteConfigs {
			result[toolName] = domain.RouteConfig{
				Method:  rc.Method,
				BaseURL: ds.BaseURL,
				Path:    rc.Path,
			}
		}
	}
	return result
}

// doGet is a generic helper that sends an authenticated GET and unmarshals the response.
func doGet[T any](ctx context.Context, client *http.Client, endpoint, token string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}

	var result T
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &result, nil
}
