package helix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	url2 "net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIBaseURL is the base URL for composing API requests.
	DefaultAPIBaseURL = "https://api.twitch.tv/helix"

	// AuthBaseURL is the base URL for composing authentication requests.
	AuthBaseURL = "https://id.twitch.tv/oauth2"
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Client struct {
	mu           sync.RWMutex
	ctx          context.Context
	opts         *Options
	lastResponse *Response
	callbacks    struct {
		onUserAccessTokenRefreshed func(newAccessToken, newRefreshToken string)
	}
}

type Options struct {
	ClientID        string
	ClientSecret    string
	AppAccessToken  string
	UserAccessToken string
	RefreshToken    string
	UserAgent       string
	RedirectURI     string
	HTTPClient      HTTPClient
	RateLimitFunc   RateLimitFunc
	APIBaseURL      string
	ExtensionOpts   ExtensionOptions
}

type ExtensionOptions struct {
	OwnerUserID    string
	Secret         string
	SignedJWTToken string
}

// DateRange is a generic struct used by various responses.
type DateRange struct {
	StartedAt Time `json:"started_at"`
	EndedAt   Time `json:"ended_at"`
}

type RateLimitFunc func(*Response) error

type ResponseCommon struct {
	StatusCode   int
	Header       http.Header
	Error        string `json:"error"`
	ErrorStatus  int    `json:"status"`
	ErrorMessage string `json:"message"`
}

func (rc *ResponseCommon) convertHeaderToInt(str string) int {
	i, _ := strconv.Atoi(str)

	return i
}

// GetRateLimit returns the "RateLimit-Limit" header as an int.
func (rc *ResponseCommon) GetRateLimit() int {
	return rc.convertHeaderToInt(rc.Header.Get("RateLimit-Limit"))
}

// GetRateLimitRemaining returns the "RateLimit-Remaining" header as an int.
func (rc *ResponseCommon) GetRateLimitRemaining() int {
	return rc.convertHeaderToInt(rc.Header.Get("RateLimit-Remaining"))
}

// GetRateLimitReset returns the "RateLimit-Reset" header as an int.
func (rc *ResponseCommon) GetRateLimitReset() int {
	return rc.convertHeaderToInt(rc.Header.Get("RateLimit-Reset"))
}

type Response struct {
	ResponseCommon
	Data interface{}
}

// HydrateResponseCommon copies the content of the source response's ResponseCommon to the supplied ResponseCommon argument
func (r *Response) HydrateResponseCommon(rc *ResponseCommon) {
	rc.StatusCode = r.ResponseCommon.StatusCode
	rc.Header = r.ResponseCommon.Header
	rc.Error = r.ResponseCommon.Error
	rc.ErrorStatus = r.ResponseCommon.ErrorStatus
	rc.ErrorMessage = r.ResponseCommon.ErrorMessage
}

type Pagination struct {
	Cursor string `json:"cursor"`
}

// NewClient returns a new Twitch Helix API client. It returns an
// if clientID is an empty string. It is concurrency safe.
func NewClient(options *Options) (*Client, error) {
	return NewClientWithContext(context.Background(), options)
}

func NewClientWithContext(ctx context.Context, options *Options) (*Client, error) {
	if options.ClientID == "" {
		return nil, errors.New("A client ID was not provided but is required")
	}

	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}

	if options.APIBaseURL == "" {
		options.APIBaseURL = DefaultAPIBaseURL
	}

	client := &Client{
		ctx:  ctx,
		opts: options,
	}

	return client, nil
}

func (c *Client) get(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodGet, path, respData, reqData, "query")
}

func (c *Client) post(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodPost, path, respData, reqData, "query")
}

func (c *Client) put(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodPut, path, respData, reqData, "query")
}

func (c *Client) delete(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodDelete, path, respData, reqData, "query")
}

func (c *Client) patchAsJSON(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodPatch, path, respData, reqData, "json")
}

func (c *Client) postAsJSON(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodPost, path, respData, reqData, "json")
}

func (c *Client) putAsJSON(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodPut, path, respData, reqData, "json")
}

func (c *Client) postAsForm(path string, respData, reqData interface{}) (*Response, error) {
	return c.sendRequest(http.MethodPost, path, respData, reqData, "form")
}

func (c *Client) sendRequest(method, path string, respData, reqData interface{}, bodyType string) (*Response, error) {
	resp := &Response{}
	if respData != nil {
		resp.Data = respData
	}

	req, err := c.newRequest(method, path, reqData, bodyType)
	if err != nil {
		return nil, err
	}

	err = c.doRequest(req, resp)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func generateFormData(data interface{}) (url2.Values, error) {
	isNil, err := isZero(data)
	if err != nil {
		return nil, err
	}

	if isNil {
		return nil, nil
	}

	formData := url2.Values{}

	vType := reflect.TypeOf(data).Elem()
	vValue := reflect.ValueOf(data).Elem()

	for i := 0; i < vType.NumField(); i++ {
		field := vType.Field(i)
		tag := field.Tag.Get("form")

		if tag == "" {
			continue
		}

		fieldVal := vValue.Field(i)
		fieldValStr := fmt.Sprintf("%v", fieldVal)

		formData.Add(tag, fieldValStr)
	}

	return formData, nil
}

func buildQueryString(req *http.Request, v interface{}) (string, error) {
	isNil, err := isZero(v)
	if err != nil {
		return "", err
	}

	if isNil {
		return "", nil
	}

	query := req.URL.Query()
	vType := reflect.TypeOf(v).Elem()
	vValue := reflect.ValueOf(v).Elem()

	for i := 0; i < vType.NumField(); i++ {
		var defaultValue string

		field := vType.Field(i)
		tag := field.Tag.Get("query")

		if tag == "" {
			continue
		}

		// Get the default value from the struct tag
		if strings.Contains(tag, ",") {
			tagSlice := strings.Split(tag, ",")

			tag = tagSlice[0]
			defaultValue = tagSlice[1]

			if defaultValue == "omitempty" {
				defaultValue = ""
			}
		}

		if field.Type.Kind() == reflect.Slice {
			// Attach any slices as query params
			fieldVal := vValue.Field(i)
			for j := 0; j < fieldVal.Len(); j++ {
				query.Add(tag, fmt.Sprintf("%v", fieldVal.Index(j)))
			}
		} else if isDatetimeTagField(tag) {
			// Get and correctly format datetime fields, and attach them query params
			dateStr := fmt.Sprintf("%v", vValue.Field(i))

			if strings.Contains(dateStr, " m=") {
				datetimeSplit := strings.Split(dateStr, " m=")
				dateStr = datetimeSplit[0]
			}

			date, err := time.Parse(requestDateTimeFormat, dateStr)
			if err != nil {
				return "", err
			}

			// Determine if the date has been set. If it has we'll add it to the query.
			if !date.IsZero() {
				query.Add(tag, date.Format(time.RFC3339))
			}
		} else {
			// Add any scalar values as query params
			fieldVal := fmt.Sprintf("%v", vValue.Field(i))

			// If no value was set by the user, use the default
			// value specified in the struct tag.
			if fieldVal == "" || fieldVal == "0" {
				if defaultValue == "" {
					continue
				}

				fieldVal = defaultValue
			}

			query.Add(tag, fieldVal)
		}
	}

	return query.Encode(), nil
}

func isZero(v interface{}) (bool, error) {
	t := reflect.TypeOf(v)
	if !t.Comparable() {
		return false, fmt.Errorf("type is not comparable: %v", t)
	}
	return v == reflect.Zero(t).Interface(), nil
}

func (c *Client) newRequest(method, path string, data interface{}, bodyType string) (*http.Request, error) {
	url := c.getBaseURL(path) + path

	switch bodyType {
	case "json":
		return c.newJSONRequest(method, url, data)
	case "form":
		return c.newFormRequest(method, url, data)
	case "query":
		fallthrough
	default:
		return c.newStandardRequest(method, url, data)
	}
}

func (c *Client) newFormRequest(method, url string, data interface{}) (*http.Request, error) {

	formData, err := generateFormData(data)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(c.ctx, method, url, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}

	if data == nil {
		return req, nil
	}

	query, err := buildQueryString(req, data)
	if err != nil {
		return nil, err
	}

	req.URL.RawQuery = query
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return req, nil
}

func (c *Client) newStandardRequest(method, url string, data interface{}) (*http.Request, error) {
	req, err := http.NewRequestWithContext(c.ctx, method, url, nil)
	if err != nil {
		return nil, err
	}

	if data == nil {
		return req, nil
	}

	query, err := buildQueryString(req, data)
	if err != nil {
		return nil, err
	}

	req.URL.RawQuery = query

	return req, nil
}

func (c *Client) newJSONRequest(method, url string, data interface{}) (*http.Request, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	buf := bytes.NewBuffer(b)

	req, err := http.NewRequestWithContext(c.ctx, method, url, buf)
	if err != nil {
		return nil, err
	}

	query, err := buildQueryString(req, data)
	if err != nil {
		return nil, err
	}

	req.URL.RawQuery = query

	req.Header.Set("Content-Type", "application/json")

	return req, nil
}

func (c *Client) getBaseURL(path string) string {
	for _, authPath := range authPaths {
		if strings.Contains(path, authPath) {
			return AuthBaseURL
		}
	}

	return c.opts.APIBaseURL
}

func (c *Client) doRequest(req *http.Request, resp *Response) error {
	c.setRequestHeaders(req)

	rateLimitFunc := c.opts.RateLimitFunc

	for {
		if c.lastResponse != nil && rateLimitFunc != nil {
			err := rateLimitFunc(c.lastResponse)
			if err != nil {
				return err
			}
		}

		response, err := c.opts.HTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("Failed to execute API request: %s", err.Error())
		}
		defer response.Body.Close()

		resp.Header = response.Header

		setResponseStatusCode(resp, "StatusCode", response.StatusCode)

		bodyBytes, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}

		// Only attempt to decode the response if we have a response we can handle
		if len(bodyBytes) > 0 && resp.StatusCode < http.StatusInternalServerError {
			if resp.Data != nil && resp.StatusCode < http.StatusBadRequest {
				// Successful request
				err = json.Unmarshal(bodyBytes, &resp.Data)
			} else {
				// A 401 means Twitch wants us to refresh our token:
				// https://dev.twitch.tv/docs/authentication/refresh-tokens/
				if resp.StatusCode == http.StatusUnauthorized && c.canRefreshToken() {
					if refreshErr := c.refreshToken(); refreshErr != nil {
						log.Printf("Failed to refresh helix auth token: %v", refreshErr)
						return err
					}
					// Try again now that we have a new token
					c.setRequestHeaders(req)
					continue
				}

				// Failed request
				err = json.Unmarshal(bodyBytes, &resp)
			}

			if err != nil {
				return fmt.Errorf("Failed to decode API response: %s", err.Error())
			}
		}

		if rateLimitFunc == nil {
			break
		} else {
			c.mu.Lock()
			c.lastResponse = resp
			c.mu.Unlock()

			if rateLimitFunc != nil &&
				c.lastResponse.StatusCode == http.StatusTooManyRequests {
				// Rate limit exceeded, retry to send request after
				// applying rate limiter callback
				continue
			}

			break
		}
	}

	return nil
}

func (c *Client) canRefreshToken() bool {
	return c.opts.ClientID != "" &&
		c.opts.ClientSecret != "" &&
		c.opts.UserAccessToken != "" &&
		c.opts.RefreshToken != ""
}

func (c *Client) refreshToken() error {
	resp, err := c.RefreshUserAccessToken(c.opts.RefreshToken)
	if err != nil || resp.StatusCode != http.StatusOK {
		statusCode := -1
		var errorMessage string
		if resp != nil {
			statusCode = resp.StatusCode
			errorMessage = resp.ErrorMessage
		}
		return fmt.Errorf("failed to refresh token: (%d: %s) %v", statusCode, errorMessage, err)
	}

	c.mu.Lock()
	c.opts.UserAccessToken = resp.Data.AccessToken
	c.opts.RefreshToken = resp.Data.RefreshToken
	c.mu.Unlock()

	if cb := c.callbacks.onUserAccessTokenRefreshed; cb != nil {
		go cb(resp.Data.AccessToken, resp.Data.RefreshToken)
	}

	return nil
}

func (c *Client) setRequestHeaders(req *http.Request) {
	opts := c.opts

	req.Header.Set("Client-ID", opts.ClientID)

	if opts.UserAgent != "" {
		req.Header.Set("User-Agent", opts.UserAgent)
	}

	var bearerToken string
	if opts.AppAccessToken != "" {
		bearerToken = opts.AppAccessToken
	}
	if opts.UserAccessToken != "" {
		bearerToken = opts.UserAccessToken
	}
	if opts.ExtensionOpts.SignedJWTToken != "" {
		bearerToken = opts.ExtensionOpts.SignedJWTToken
	}

	authType := "Bearer"
	// Token validation requires different type of Auth
	if req.URL.String() == AuthBaseURL+authPaths["validate"] {
		authType = "OAuth"
	}

	if bearerToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("%s %s", authType, bearerToken))
	}
}

func setResponseStatusCode(v interface{}, fieldName string, code int) {
	s := reflect.ValueOf(v).Elem()
	field := s.FieldByName(fieldName)
	field.SetInt(int64(code))
}

// GetAppAccessToken returns the current app access token.
func (c *Client) GetAppAccessToken() string {
	return c.opts.AppAccessToken
}

func (c *Client) SetAppAccessToken(accessToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.AppAccessToken = accessToken
}

// GetUserAccessToken returns the current user access token.
func (c *Client) GetUserAccessToken() string {
	return c.opts.UserAccessToken
}

func (c *Client) SetUserAccessToken(accessToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.UserAccessToken = accessToken
}

// GetRefreshToken returns the current refresh token.
func (c *Client) GetRefreshToken() string {
	return c.opts.RefreshToken
}

func (c *Client) SetRefreshToken(refreshToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.RefreshToken = refreshToken
}

// GetAppAccessToken returns the current app access token.
func (c *Client) GetExtensionSignedJWTToken() string {
	return c.opts.ExtensionOpts.SignedJWTToken
}

func (c *Client) SetExtensionSignedJWTToken(jwt string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.ExtensionOpts.SignedJWTToken = jwt
}

func (c *Client) SetUserAgent(userAgent string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.UserAgent = userAgent
}

func (c *Client) SetRedirectURI(uri string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.RedirectURI = uri
}

func (c *Client) OnUserAccessTokenRefreshed(f func(newAccessToken, newRefreshToken string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callbacks.onUserAccessTokenRefreshed = f
}
