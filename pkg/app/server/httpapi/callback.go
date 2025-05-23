// Copyright 2024 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/xsrftoken"

	"github.com/pipe-cd/pipecd/pkg/jwt"
	"github.com/pipe-cd/pipecd/pkg/model"
	"github.com/pipe-cd/pipecd/pkg/oauth/github"
	"github.com/pipe-cd/pipecd/pkg/oauth/oidc"
)

func (h *authHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")

	// Validate request's payload.

	// split the project ID from the state, if it exists.
	// This is necessary because some providers don't support passing the project ID in the query parameters.
	state, projectID, err := parseProjectAndState(r)
	if err != nil {
		h.handleError(w, r, "Failed to parse state", err)
		return
	}

	if err := checkState(r, h.stateKey, state); err != nil {
		h.handleError(w, r, "Unauthorized access", err)
		return
	}

	authCode := r.FormValue(authCodeFormKey)
	if authCode == "" {
		h.handleError(w, r, "Missing auth code", nil)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	proj, err := h.projectGetter.Get(ctx, projectID)
	if err != nil {
		h.handleError(w, r, fmt.Sprintf("Unable to find project %s", projectID), err)
		return
	}

	if proj.UserGroups == nil {
		h.handleError(w, r, "Missing User Group configuration", nil)
		return
	}

	sso, shared, err := h.findSSOConfig(proj)
	if err != nil {
		h.handleError(w, r, fmt.Sprintf("Invalid SSO configuration: %v", err), nil)
		return
	}
	sessionTTLFromConfig := sso.SessionTtl
	var tokenTTL time.Duration
	if sessionTTLFromConfig == 0 {
		tokenTTL = defaultTokenTTL
	} else {
		tokenTTL = time.Duration(sessionTTLFromConfig) * time.Hour
	}

	if !shared {
		if err := sso.Decrypt(h.decrypter); err != nil {
			h.handleError(w, r, "Failed to decrypt SSO configuration", err)
			return
		}
	}
	user, err := getUser(ctx, sso, proj, authCode)
	if err != nil {
		h.handleError(w, r, "Unable to find user", err)
		return
	}

	claims := jwt.NewClaims(
		user.Username,
		user.AvatarUrl,
		tokenTTL,
		*user.Role,
	)
	signedToken, err := h.signer.Sign(claims)
	if err != nil {
		h.handleError(w, r, "Internal error", err)
		return
	}

	h.logger.Info("user logged in",
		zap.String("user", user.Username),
		zap.String("project-id", proj.Id),
		zap.String("project-role", user.Role.String()),
	)

	http.SetCookie(w, makeTokenCookie(signedToken, true))
	http.SetCookie(w, makeExpiredStateCookie(h.secureCookie))
	http.Redirect(w, r, rootPath, http.StatusFound)
}

func checkState(r *http.Request, key string, state string) error {
	rawStateToken, err := hex.DecodeString(state)
	if err != nil {
		return err
	}

	stateToken := string(rawStateToken)
	if !xsrftoken.Valid(stateToken, key, "", "") {
		return fmt.Errorf("invalid state")
	}

	c, err := r.Cookie(stateCookieKey)
	if err != nil {
		return err
	}

	secretState := c.Value
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(secretState)) != 1 {
		return fmt.Errorf("wrong state")
	}

	return nil
}

func getUser(ctx context.Context, sso *model.ProjectSSOConfig, project *model.Project, code string) (*model.User, error) {
	switch sso.Provider {
	case model.ProjectSSOConfig_GITHUB:
		if sso.Github == nil {
			return nil, fmt.Errorf("missing GitHub oauth in the SSO configuration")
		}
		cli, err := github.NewOAuthClient(ctx, sso.Github, project, code)
		if err != nil {
			return nil, err
		}
		return cli.GetUser(ctx)
	case model.ProjectSSOConfig_OIDC:
		if sso.Oidc == nil {
			return nil, fmt.Errorf("missing OIDC oauth in the SSO configuration")
		}
		cli, err := oidc.NewOAuthClient(ctx, sso.Oidc, project, code)
		if err != nil {
			return nil, err
		}
		return cli.GetUser(ctx)
	default:
		return nil, fmt.Errorf("not implemented")
	}
}

func parseProjectAndState(r *http.Request) (string, string, error) {
	state := r.FormValue(stateFormKey)
	if state == "" {
		return "", "", fmt.Errorf("missing state")
	}

	// When using OIDC SSO, the state is in the format of "state-token:project-id".
	s := strings.Split(state, ":")
	if len(s) != 2 {
		projectID := r.FormValue(projectFormKey)
		if projectID == "" {
			return s[0], "", fmt.Errorf("missing project id")
		}
		return state, projectID, nil
	} else {
		if s[1] == "" {
			return s[0], "", fmt.Errorf("missing project id")
		}
		return s[0], s[1], nil
	}
}
