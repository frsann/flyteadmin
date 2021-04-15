package authzserver

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/flyteorg/flyteadmin/pkg/auth"

	authConfig "github.com/flyteorg/flyteadmin/pkg/auth/config"

	"github.com/flyteorg/flyteidl/gen/pb-go/flyteidl/service"
)

type OAuth2MetadataProvider struct {
	cfg *authConfig.Config
}

func (s OAuth2MetadataProvider) OAuth2Metadata(context.Context, *service.OAuth2MetadataRequest) (*service.OAuth2MetadataResponse, error) {
	switch s.cfg.AppAuth.AuthServerType {
	case authConfig.AuthorizationServerTypeSelf:
		doc := &service.OAuth2MetadataResponse{
			Issuer:                        GetIssuer(s.cfg),
			AuthorizationEndpoint:         s.cfg.HTTPPublicUri.ResolveReference(authorizeRelativeUrl).String(),
			TokenEndpoint:                 s.cfg.HTTPPublicUri.ResolveReference(tokenRelativeUrl).String(),
			JwksUri:                       s.cfg.HTTPPublicUri.ResolveReference(jsonWebKeysUrl).String(),
			CodeChallengeMethodsSupported: []string{"S256"},
			ResponseTypesSupported: []string{
				"code",
				"token",
				"code token",
			},
			GrantTypesSupported: supportedGrantTypes,
			ScopesSupported:     []string{auth.ScopeAll},
			TokenEndpointAuthMethodsSupported: []string{
				"client_secret_basic",
			},
		}

		return doc, nil
	default:
		var externalMetadataURL *url.URL
		if len(s.cfg.AppAuth.ExternalAuthServer.BaseURL.String()) > 0 {
			externalMetadataURL = s.cfg.AppAuth.ExternalAuthServer.BaseURL.ResolveReference(oauth2MetadataEndpoint)
		} else {
			externalMetadataURL = s.cfg.UserAuth.OpenID.BaseURL.ResolveReference(oauth2MetadataEndpoint)
		}

		response, err := http.Get(externalMetadataURL.String())
		if err != nil {
			return nil, err
		}

		raw, err := ioutil.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}

		resp := &service.OAuth2MetadataResponse{}
		err = unmarshalResp(response, raw, resp)
		if err != nil {
			return nil, err
		}

		return resp, nil
	}
}

func (s OAuth2MetadataProvider) FlyteClient(context.Context, *service.FlyteClientRequest) (*service.FlyteClientResponse, error) {
	return &service.FlyteClientResponse{
		ClientId:                 s.cfg.AppAuth.ThirdParty.FlyteClientConfig.ClientID,
		RedirectUri:              s.cfg.AppAuth.ThirdParty.FlyteClientConfig.RedirectURI,
		Scopes:                   s.cfg.AppAuth.ThirdParty.FlyteClientConfig.Scopes,
		AuthorizationMetadataKey: s.cfg.GrpcAuthorizationHeader,
	}, nil
}

func NewService(config *authConfig.Config) OAuth2MetadataProvider {
	return OAuth2MetadataProvider{
		cfg: config,
	}
}
