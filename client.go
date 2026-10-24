package simplydotcom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// simplyClient defines the methods that any client implementation must provide
type simplyClient interface {
	getDnsRecords(ctx context.Context, zone string) ([]dnsRecordResponse, error)
	addDnsRecord(ctx context.Context, zone string, record dnsRecord) (createRecordResponse, error)
	updateDnsRecord(ctx context.Context, zone string, recordID int, record dnsRecord) error
	deleteDnsRecord(ctx context.Context, zone string, recordID int) error
}

// simplyApiClient handles API communication with Simply.com
type simplyApiClient struct {
	apiKey     string
	baseURL    string
	maxRetries int
	httpClient *http.Client

	productsMu     sync.Mutex
	productObjects map[string]string
}

// buildPath joins the base URL with the provided path segments
func (c *simplyApiClient) buildPath(segments ...string) (string, error) {
	return url.JoinPath(c.baseURL, segments...)
}

// buildProductPath builds the URL of the product that holds zone, followed by
// the provided path segments. The last segment must end with a slash, as the
// API expects a trailing slash.
func (c *simplyApiClient) buildProductPath(ctx context.Context, zone string, segments ...string) (string, error) {
	object, err := c.productObject(ctx, zone)
	if err != nil {
		return "", err
	}
	return c.buildPath(append([]string{"my/products", url.PathEscape(object)}, segments...)...)
}

// productObject returns the product object (handle) for zone. The API
// expects the object in the path, which is the Unicode form for IDN domains,
// so the zone is matched against the object and both the Unicode and the
// punycode domain name of each product on the account.
func (c *simplyApiClient) productObject(ctx context.Context, zone string) (string, error) {
	c.productsMu.Lock()
	defer c.productsMu.Unlock()

	key := strings.ToLower(trimTrailingDot(zone))
	if object, ok := c.productObjects[key]; ok {
		return object, nil
	}

	reqURL, err := c.buildPath("my/products/")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	var response productsResponse
	if err := c.doApiRequest(req, &response); err != nil {
		return "", fmt.Errorf("failed to list products: %w", err)
	}

	objects := make(map[string]string, len(response.Products)*3)
	for _, p := range response.Products {
		objects[strings.ToLower(p.Object)] = p.Object
	}
	for _, p := range response.Products {
		for _, name := range []string{p.Domain.Name, p.Domain.NameIdn} {
			nameKey := strings.ToLower(name)
			if _, exists := objects[nameKey]; name != "" && !exists {
				objects[nameKey] = p.Object
			}
		}
	}
	c.productObjects = objects

	if object, ok := objects[key]; ok {
		return object, nil
	}
	return "", fmt.Errorf("zone %s not found on the Simply.com account", trimTrailingDot(zone))
}

func (c *simplyApiClient) addDnsRecord(ctx context.Context, zone string, record dnsRecord) (createRecordResponse, error) {
	reqURL, err := c.buildProductPath(ctx, zone, "dns/records/")
	if err != nil {
		return createRecordResponse{}, err
	}

	jsonBytes, err := json.Marshal(record)
	if err != nil {
		return createRecordResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return createRecordResponse{}, err
	}

	req.Header.Set("Content-Type", "application/json")

	var response createRecordResponse
	err = c.doApiRequest(req, &response)
	if err != nil {
		return createRecordResponse{}, err
	}
	return response, nil
}

func (c *simplyApiClient) getDnsRecords(ctx context.Context, zone string) ([]dnsRecordResponse, error) {
	var result getRecordsResponse
	reqURL, err := c.buildProductPath(ctx, zone, "dns/records/")
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	err = c.doApiRequest(req, &result)
	if err != nil {
		return nil, err
	}

	return result.Records, nil
}

// deleteDnsRecord deletes a single DNS record by ID
func (c *simplyApiClient) deleteDnsRecord(ctx context.Context, zone string, recordID int) error {
	reqURL, err := c.buildProductPath(ctx, zone, "dns/records", fmt.Sprintf("%d/", recordID))
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return err
	}

	var response simplyResponse
	err = c.doApiRequest(req, &response)
	if err != nil {
		return fmt.Errorf("failed to delete DNS record %d: %w", recordID, err)
	}

	return nil
}

// updateDnsRecord updates a single DNS record
func (c *simplyApiClient) updateDnsRecord(ctx context.Context, zone string, recordID int, record dnsRecord) error {
	reqURL, err := c.buildProductPath(ctx, zone, "dns/records", fmt.Sprintf("%d/", recordID))
	if err != nil {
		return err
	}

	jsonBytes, err := json.Marshal(record)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	var response simplyResponse
	err = c.doApiRequest(req, &response)
	if err != nil {
		return fmt.Errorf("failed to update DNS record %d: %w", recordID, err)
	}

	return nil
}

// trimTrailingDot removes a trailing dot from a domain name if present
func trimTrailingDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}

// doApiRequest does the round trip, adding Authorization header if not already supplied.
// It returns the decoded response if successful; otherwise it returns an error.
// If the API returns a 429 rate limit response, it will retry the request after the
// specified delay up to maxRetries times.
func (c *simplyApiClient) doApiRequest(req *http.Request, result any) error {
	if req.Header.Get("Authorization") == "" {
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	}
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	var retryCount int

	for {
		// For non-GET requests that have a body, we need to restore the body for each retry
		if req.Body != nil && req.Method != http.MethodGet {
			if req.GetBody == nil {
				return fmt.Errorf("request body cannot be reused for retries")
			}
			reqBody, err := req.GetBody()
			if err != nil {
				return fmt.Errorf("failed to get request body for retry: %w", err)
			}
			req.Body = reqBody
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}

		// Handle rate limiting (HTTP 429)
		if resp.StatusCode == http.StatusTooManyRequests && (c.maxRetries < 0 || retryCount < c.maxRetries) {
			resp.Body.Close() // Close the body before retry

			retryCount++

			// Parse retry-after header
			retryAfterHeader := resp.Header.Get("x-ratelimit-retry-after")
			var retryAfterSec int
			if retryAfterHeader != "" {
				fmt.Sscanf(retryAfterHeader, "%d", &retryAfterSec)
			}

			// If no valid retry-after header, use exponential backoff
			if retryAfterSec <= 0 {
				retryAfterSec = 1 << retryCount // 2, 4, 8 seconds
			}

			// Create timer before select to avoid leaking timer goroutine
			timer := time.NewTimer(time.Duration(retryAfterSec) * time.Second)
			defer timer.Stop()

			// Wait before retrying
			select {
			case <-req.Context().Done():
				return req.Context().Err()
			case <-timer.C:
				continue
			}
		}

		// Handle other status codes
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return parseError(resp)
		}

		err = json.NewDecoder(resp.Body).Decode(&result)
		if err != nil {
			return err
		}

		if result != nil {
			return nil
		}

		return fmt.Errorf("got empty response")
	}
}

func parseError(resp *http.Response) error {
	var errorResponse simplyResponse
	err := json.NewDecoder(resp.Body).Decode(&errorResponse)
	if err != nil || errorResponse.Message == "" {
		return fmt.Errorf("server returned HTTP error %d", resp.StatusCode)
	}

	return fmt.Errorf("%s", errorResponse.Message)
}
