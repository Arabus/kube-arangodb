//
// DISCLAIMER
//
// Copyright 2017 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	driver "github.com/arangodb/go-driver"
	"github.com/arangodb/go-driver/cluster"
	"github.com/arangodb/go-driver/util"
	velocypack "github.com/arangodb/go-velocypack"
)

const (
	DefaultMaxIdleConnsPerHost = 64

	keyRawResponse driver.ContextKey = "arangodb-rawResponse"
	keyResponse    driver.ContextKey = "arangodb-response"
)

// ConnectionConfig provides all configuration options for a HTTP connection.
type ConnectionConfig struct {
	// Endpoints holds 1 or more URL's used to connect to the database.
	// In case of a connection to an ArangoDB cluster, you must provide the URL's of all coordinators.
	Endpoints []string
	// TLSConfig holds settings used to configure a TLS (HTTPS) connection.
	// This is only used for endpoints using the HTTPS scheme.
	TLSConfig *tls.Config
	// Transport allows the use of a custom round tripper.
	// If Transport is not of type `*http.Transport`, the `TLSConfig` property is not used.
	// Otherwise a `TLSConfig` property other than `nil` will overwrite the `TLSClientConfig`
	// property of `Transport`.
	//
	// When using a custom `http.Transport`, make sure to set the `MaxIdleConnsPerHost` field at least as
	// high as the maximum number of concurrent requests you will make to your database.
	// A lower number will cause the golang runtime to create additional connections and close them
	// directly after use, resulting in a large number of connections in `TIME_WAIT` state.
	// When this value is not set, the driver will set it to 64 automatically.
	Transport http.RoundTripper
	// FailOnRedirect; if set, redirect will not be followed, instead the status code is returned as error
	FailOnRedirect bool
	// Cluster configuration settings
	cluster.ConnectionConfig
	// ContentType specified type of content encoding to use.
	ContentType driver.ContentType
}

// NewConnection creates a new HTTP connection based on the given configuration settings.
func NewConnection(config ConnectionConfig) (driver.Connection, error) {
	c, err := cluster.NewConnection(config.ConnectionConfig, func(endpoint string) (driver.Connection, error) {
		conn, err := newHTTPConnection(endpoint, config)
		if err != nil {
			return nil, driver.WithStack(err)
		}
		return conn, nil
	}, config.Endpoints)
	if err != nil {
		return nil, driver.WithStack(err)
	}
	return c, nil
}

// newHTTPConnection creates a new HTTP connection for a single endpoint and the remainder of the given configuration settings.
func newHTTPConnection(endpoint string, config ConnectionConfig) (driver.Connection, error) {
	endpoint = util.FixupEndpointURLScheme(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, driver.WithStack(err)
	}
	var httpTransport *http.Transport
	if config.Transport != nil {
		httpTransport, _ = config.Transport.(*http.Transport)
	} else {
		httpTransport = &http.Transport{
			// Copy default values from http.DefaultTransport
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		config.Transport = httpTransport
	}
	if httpTransport != nil {
		if httpTransport.MaxIdleConnsPerHost == 0 {
			// Raise the default number of idle connections per host since in a database application
			// it is very likely that you want more than 2 concurrent connections to a host.
			// We raise it to avoid the extra concurrent connections being closed directly
			// after use, resulting in a lot of connection in `TIME_WAIT` state.
			httpTransport.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
		}
		defaultMaxIdleConns := 3 * DefaultMaxIdleConnsPerHost
		if httpTransport.MaxIdleConns > 0 && httpTransport.MaxIdleConns < defaultMaxIdleConns {
			// For a cluster scenario we assume the use of 3 coordinators (don't know the exact number here)
			// and derive the maximum total number of idle connections from that.
			httpTransport.MaxIdleConns = defaultMaxIdleConns
		}
		if config.TLSConfig != nil {
			httpTransport.TLSClientConfig = config.TLSConfig
		}
	}
	httpClient := &http.Client{
		Transport: config.Transport,
	}
	if config.FailOnRedirect {
		httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return driver.ArangoError{
				HasError:     true,
				Code:         http.StatusFound,
				ErrorNum:     0,
				ErrorMessage: "Redirect not allowed",
			}
		}
	}
	c := &httpConnection{
		endpoint:    *u,
		contentType: config.ContentType,
		client:      httpClient,
	}
	return c, nil
}

// httpConnection implements an HTTP + JSON connection to an arangodb server.
type httpConnection struct {
	endpoint    url.URL
	contentType driver.ContentType
	client      *http.Client
}

// String returns the endpoint as string
func (c *httpConnection) String() string {
	return c.endpoint.String()
}

// NewRequest creates a new request with given method and path.
func (c *httpConnection) NewRequest(method, path string) (driver.Request, error) {
	switch method {
	case "GET", "POST", "DELETE", "HEAD", "PATCH", "PUT", "OPTIONS":
	// Ok
	default:
		return nil, driver.WithStack(driver.InvalidArgumentError{Message: fmt.Sprintf("Invalid method '%s'", method)})
	}
	ct := c.contentType
	if ct != driver.ContentTypeJSON && strings.Contains(path, "_api/gharial") {
		// Currently (3.1.18) calls to this API do not work well with vpack.
		ct = driver.ContentTypeJSON
	}
	switch ct {
	case driver.ContentTypeJSON:
		r := &httpJSONRequest{
			method: method,
			path:   path,
		}
		return r, nil
	case driver.ContentTypeVelocypack:
		r := &httpVPackRequest{
			method: method,
			path:   path,
		}
		return r, nil
	default:
		return nil, driver.WithStack(fmt.Errorf("Unsupported content type %d", int(c.contentType)))
	}
}

// Do performs a given request, returning its response.
func (c *httpConnection) Do(ctx context.Context, req driver.Request) (driver.Response, error) {
	httpReq, ok := req.(httpRequest)
	if !ok {
		return nil, driver.WithStack(driver.InvalidArgumentError{Message: "request is not a httpRequest"})
	}
	r, err := httpReq.createHTTPRequest(c.endpoint)
	rctx := ctx
	if rctx == nil {
		rctx = context.Background()
	}
	rctx = httptrace.WithClientTrace(rctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			httpReq.WroteRequest(info)
		},
	})
	r = r.WithContext(rctx)
	if err != nil {
		return nil, driver.WithStack(err)
	}
	resp, err := c.client.Do(r)
	if err != nil {
		return nil, driver.WithStack(err)
	}
	var rawResponse *[]byte
	if ctx != nil {
		if v := ctx.Value(keyRawResponse); v != nil {
			if buf, ok := v.(*[]byte); ok {
				rawResponse = buf
			}
		}
	}

	// Read response body
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, driver.WithStack(err)
	}
	if rawResponse != nil {
		*rawResponse = body
	}

	ct := resp.Header.Get("Content-Type")
	var httpResp driver.Response
	switch strings.Split(ct, ";")[0] {
	case "application/json", "application/x-arango-dump":
		httpResp = &httpJSONResponse{resp: resp, rawResponse: body}
	case "application/x-velocypack":
		httpResp = &httpVPackResponse{resp: resp, rawResponse: body}
	default:
		if resp.StatusCode == http.StatusUnauthorized {
			// When unauthorized the server sometimes return a `text/plain` response.
			return nil, driver.WithStack(driver.ArangoError{
				HasError:     true,
				Code:         resp.StatusCode,
				ErrorMessage: string(body),
			})
		}
		// Handle empty 'text/plain' body as empty JSON object
		if len(body) == 0 {
			body = []byte("{}")
			if rawResponse != nil {
				*rawResponse = body
			}
			httpResp = &httpJSONResponse{resp: resp, rawResponse: body}
		} else {
			return nil, driver.WithStack(fmt.Errorf("Unsupported content type '%s' with status %d and content '%s'", ct, resp.StatusCode, string(body)))
		}
	}
	if ctx != nil {
		if v := ctx.Value(keyResponse); v != nil {
			if respPtr, ok := v.(*driver.Response); ok {
				*respPtr = httpResp
			}
		}
	}
	return httpResp, nil
}

// Unmarshal unmarshals the given raw object into the given result interface.
func (c *httpConnection) Unmarshal(data driver.RawObject, result interface{}) error {
	ct := c.contentType
	if ct == driver.ContentTypeVelocypack && len(data) >= 2 {
		// Poor mans auto detection of json
		l := len(data)
		if (data[0] == '{' && data[l-1] == '}') || (data[0] == '[' && data[l-1] == ']') {
			ct = driver.ContentTypeJSON
		}
	}
	switch ct {
	case driver.ContentTypeJSON:
		if err := json.Unmarshal(data, result); err != nil {
			return driver.WithStack(err)
		}
	case driver.ContentTypeVelocypack:
		//panic(velocypack.Slice(data))
		if err := velocypack.Unmarshal(velocypack.Slice(data), result); err != nil {
			return driver.WithStack(err)
		}
	default:
		return driver.WithStack(fmt.Errorf("Unsupported content type %d", int(c.contentType)))
	}
	return nil
}

// Endpoints returns the endpoints used by this connection.
func (c *httpConnection) Endpoints() []string {
	return []string{c.endpoint.String()}
}

// UpdateEndpoints reconfigures the connection to use the given endpoints.
func (c *httpConnection) UpdateEndpoints(endpoints []string) error {
	// Do nothing here.
	// The real updating is done in cluster Connection.
	return nil
}

// Configure the authentication used for this connection.
func (c *httpConnection) SetAuthentication(auth driver.Authentication) (driver.Connection, error) {
	var httpAuth httpAuthentication
	switch auth.Type() {
	case driver.AuthenticationTypeBasic:
		userName := auth.Get("username")
		password := auth.Get("password")
		httpAuth = newBasicAuthentication(userName, password)
	case driver.AuthenticationTypeJWT:
		userName := auth.Get("username")
		password := auth.Get("password")
		httpAuth = newJWTAuthentication(userName, password)
	case driver.AuthenticationTypeRaw:
		value := auth.Get("value")
		httpAuth = newRawAuthentication(value)
	default:
		return nil, driver.WithStack(fmt.Errorf("Unsupported authentication type %d", int(auth.Type())))
	}

	result, err := newAuthenticatedConnection(c, httpAuth)
	if err != nil {
		return nil, driver.WithStack(err)
	}
	return result, nil
}

// Protocols returns all protocols used by this connection.
func (c *httpConnection) Protocols() driver.ProtocolSet {
	return driver.ProtocolSet{driver.ProtocolHTTP}
}
