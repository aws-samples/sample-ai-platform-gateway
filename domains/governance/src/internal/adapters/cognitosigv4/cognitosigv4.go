// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package cognitosigv4 implements ports.Identity against Cognito using
// SigV4-signed HTTP (aws/signer/v4) — WITHOUT the Cognito SDK, exactly the
// mechanism already used by config-api and signup. Fewer dependencies, smaller
// binary. This package is the ONLY part that knows how to talk to Cognito.
package cognitosigv4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aiplat/governance/internal/ports"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Adapter talks to Cognito over SigV4 HTTP.
type Adapter struct {
	cfg      aws.Config
	http     *http.Client
	userPool string
}

// New builds the adapter. When hc is nil it uses an http.Client with a 15s
// timeout (the same default as config-api).
func New(cfg aws.Config, userPool string, hc *http.Client) *Adapter {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Adapter{cfg: cfg, http: hc, userPool: userPool}
}

var _ ports.Identity = (*Adapter)(nil)

func (a *Adapter) host() string { return "cognito-idp." + a.cfg.Region + ".amazonaws.com" }

// jsonPost: signed (SigV4) JSON-1.1 call. Identical to config-api's awsJSONPost.
func (a *Adapter) jsonPost(ctx context.Context, target string, payload interface{}) ([]byte, int, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+a.host()+"/", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("content-type", "application/x-amz-json-1.1")
	req.Header.Set("x-amz-target", target)
	creds, err := a.cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, 0, err
	}
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "cognito-idp", a.cfg.Region, time.Now()); err != nil {
		return nil, 0, err
	}
	res, err := a.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(res.Body)
	return rb, res.StatusCode, nil
}

func (a *Adapter) ListUsers(ctx context.Context, org string) ([]ports.User, error) {
	if a.userPool == "" {
		return nil, errors.New("user pool is not configured")
	}
	var out []ports.User
	var token string
	for {
		payload := map[string]interface{}{"UserPoolId": a.userPool, "Limit": 60}
		if token != "" {
			payload["PaginationToken"] = token
		}
		rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.ListUsers", payload)
		if err != nil {
			return nil, err
		}
		if code >= 300 {
			return nil, errors.New("cognito: " + string(rb))
		}
		var r struct {
			Users []struct {
				UserStatus string                         `json:"UserStatus"`
				Enabled    bool                           `json:"Enabled"`
				Attributes []struct{ Name, Value string } `json:"Attributes"`
			} `json:"Users"`
			PaginationToken string `json:"PaginationToken"`
		}
		json.Unmarshal(rb, &r)
		for _, u := range r.Users {
			email, uorg, role := "", "", ""
			for _, at := range u.Attributes {
				switch at.Name {
				case "email":
					email = at.Value
				case "custom:org_id":
					uorg = at.Value
				case "custom:role":
					role = at.Value
				}
			}
			if uorg == org && email != "" {
				out = append(out, ports.User{Email: email, Org: uorg, Role: role, Status: u.UserStatus, Enabled: u.Enabled})
			}
		}
		if r.PaginationToken == "" {
			break
		}
		token = r.PaginationToken
	}
	return out, nil
}

func (a *Adapter) attrs(u ports.User) []map[string]string {
	attrs := []map[string]string{
		{"Name": "email", "Value": u.Email},
		{"Name": "email_verified", "Value": "true"},
		{"Name": "custom:org_id", "Value": u.Org},
		{"Name": "custom:role", "Value": u.Role},
	}
	if u.Name != "" {
		attrs = append(attrs, map[string]string{"Name": "name", "Value": u.Name})
	}
	return attrs
}

func (a *Adapter) CreateUser(ctx context.Context, u ports.User, password string) error {
	if password != "" {
		// Permanent password, no e-mail invite (the platform_admin flow).
		rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminCreateUser",
			map[string]interface{}{
				"UserPoolId":        a.userPool,
				"Username":          u.Email,
				"TemporaryPassword": password,
				"MessageAction":     "SUPPRESS",
				"UserAttributes":    a.attrs(u),
			})
		if err != nil {
			return err
		}
		if code >= 300 {
			if strings.Contains(string(rb), "UsernameExistsException") {
				return ports.ErrUserExists
			}
			return errors.New("cognito create: " + string(rb))
		}
		rb, code, err = a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminSetUserPassword",
			map[string]interface{}{
				"UserPoolId": a.userPool,
				"Username":   u.Email,
				"Password":   password,
				"Permanent":  true,
			})
		if err != nil {
			return err
		}
		if code >= 300 {
			return errors.New("cognito set password: " + string(rb))
		}
		return nil
	}
	// E-mail invite (temporary password generated by Cognito).
	rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminCreateUser",
		map[string]interface{}{
			"UserPoolId":             a.userPool,
			"Username":               u.Email,
			"DesiredDeliveryMediums": []string{"EMAIL"},
			"UserAttributes":         a.attrs(u),
		})
	if err != nil {
		return err
	}
	if code >= 300 {
		if strings.Contains(string(rb), "UsernameExistsException") {
			return ports.ErrUserExists
		}
		return errors.New("cognito: " + string(rb))
	}
	return nil
}

func (a *Adapter) UpdateAttrs(ctx context.Context, email string, attrs map[string]string) error {
	var ua []map[string]string
	for k, v := range attrs {
		ua = append(ua, map[string]string{"Name": k, "Value": v})
	}
	rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminUpdateUserAttributes",
		map[string]interface{}{"UserPoolId": a.userPool, "Username": email, "UserAttributes": ua})
	if err != nil {
		return err
	}
	if code >= 300 {
		return errors.New("cognito: " + string(rb))
	}
	return nil
}

func (a *Adapter) DeleteUser(ctx context.Context, email string) error {
	rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminDeleteUser",
		map[string]interface{}{"UserPoolId": a.userPool, "Username": email})
	if err != nil {
		return err
	}
	if code >= 300 {
		return errors.New("cognito: " + string(rb))
	}
	return nil
}

// ResetPassword triggers AdminResetUserPassword: Cognito sends a code to the
// member's e-mail and puts them in RESET_REQUIRED. The member finishes through the
// "forgot my password" screen. The admin neither sees nor sets the password.
func (a *Adapter) ResetPassword(ctx context.Context, email string) error {
	rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminResetUserPassword",
		map[string]interface{}{"UserPoolId": a.userPool, "Username": email})
	if err != nil {
		return err
	}
	if code >= 300 {
		return errors.New("cognito reset: " + string(rb))
	}
	return nil
}

// ResendInvite resends the invite (a new temporary password by e-mail) using
// AdminCreateUser with MessageAction=RESEND. It only works for someone still in
// FORCE_CHANGE_PASSWORD (pending invite).
func (a *Adapter) ResendInvite(ctx context.Context, email string) error {
	rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminCreateUser",
		map[string]interface{}{
			"UserPoolId":             a.userPool,
			"Username":               email,
			"MessageAction":          "RESEND",
			"DesiredDeliveryMediums": []string{"EMAIL"},
		})
	if err != nil {
		return err
	}
	if code >= 300 {
		return errors.New("cognito resend: " + string(rb))
	}
	return nil
}

// SetEnabled enables/disables the account (AdminEnableUser/AdminDisableUser).
// Reversible block: a disabled account cannot authenticate, but it is not deleted.
func (a *Adapter) SetEnabled(ctx context.Context, email string, enabled bool) error {
	target := "AWSCognitoIdentityProviderService.AdminDisableUser"
	if enabled {
		target = "AWSCognitoIdentityProviderService.AdminEnableUser"
	}
	rb, code, err := a.jsonPost(ctx, target,
		map[string]interface{}{"UserPoolId": a.userPool, "Username": email})
	if err != nil {
		return err
	}
	if code >= 300 {
		return errors.New("cognito enable/disable: " + string(rb))
	}
	return nil
}

func (a *Adapter) GetUserOrg(ctx context.Context, email string) (string, bool, error) {
	rb, code, err := a.jsonPost(ctx, "AWSCognitoIdentityProviderService.AdminGetUser",
		map[string]interface{}{"UserPoolId": a.userPool, "Username": email})
	if err != nil {
		return "", false, err
	}
	if code >= 300 {
		return "", false, nil
	}
	var r struct {
		UserAttributes []struct{ Name, Value string } `json:"UserAttributes"`
	}
	json.Unmarshal(rb, &r)
	for _, at := range r.UserAttributes {
		if at.Name == "custom:org_id" {
			return at.Value, true, nil
		}
	}
	return "", true, nil
}
