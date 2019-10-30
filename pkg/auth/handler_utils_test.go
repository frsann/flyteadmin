package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"net/http"
	"testing"
)

type RoundTripFunc func(request *http.Request) *http.Response

func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func NewTestHttpClient(fn RoundTripFunc) *http.Client {
	return &http.Client{
		Transport: fn,
	}
}

func TestPostToIdp(t *testing.T) {
	ctx := context.Background()
	token := "j.w.t"

	// Use a real client and a real token to run a live test
	responseObj := &UserInfoResponse{
		Sub:               "abc123",
		Name:              "John Smith",
		PreferredUsername: "jsmith@company.com",
		GivenName:         "John",
		FamilyName:        "Smith",
	}
	responseBytes, err := json.Marshal(responseObj)
	assert.NoError(t, err)
	client := NewTestHttpClient(func(request *http.Request) *http.Response {
		return &http.Response{
			StatusCode: 200,
			Body:       ioutil.NopCloser(bytes.NewReader(responseBytes)),
			Header:     make(http.Header),
		}
	})

	obj, err := postToIdp(ctx, client, "https://lyft.okta.com/oauth2/ausc5wmjw96cRKvTd1t7/v1/userinfo", token)
	assert.NoError(t, err)
	assert.Equal(t, responseObj.Name, obj.Name)
	assert.Equal(t, responseObj.Sub, obj.Sub)
	assert.Equal(t, responseObj.PreferredUsername, obj.PreferredUsername)
	assert.Equal(t, responseObj.GivenName, obj.GivenName)
	assert.Equal(t, responseObj.FamilyName, obj.FamilyName)
}

func TestPatterns(t *testing.T) {
	pattern, err := runtime.NewPattern(1, []int{3, 0}, []string{}, "")
	assert.NoError(t, err)
	fmt.Println(pattern)
	x, err := pattern.Match([]string{"api", "v1", "executions", "flytekit", "production"}, "")
	assert.NoError(t, err)
	fmt.Println(x)
}
