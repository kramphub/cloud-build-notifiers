// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/GoogleCloudPlatform/cloud-build-notifiers/lib/notifiers"
	log "github.com/golang/glog"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
)

const (
	githubTokenSecretName = "githubToken"
	githubApiEndpoint     = "https://api.github.com/repos"
)

func main() {
	if err := notifiers.Main(new(githubissuesNotifier)); err != nil {
		log.Fatalf("fatal error: %v", err)
	}
}

type githubissuesNotifier struct {
	filter      notifiers.EventFilter
	tmpl        *template.Template
	githubToken string
	githubRepo  string

	br       notifiers.BindingResolver
	tmplView *notifiers.TemplateView
}

type githubissuesMessage struct {
	Title string              `json:"title"`
	Body  *notifiers.Template `json:"body"`
}

func (g *githubissuesNotifier) SetUp(ctx context.Context, cfg *notifiers.Config, issueTemplate string, sg notifiers.SecretGetter, br notifiers.BindingResolver) error {
	prd, err := notifiers.MakeCELPredicate(cfg.Spec.Notification.Filter)
	if err != nil {
		return fmt.Errorf("failed to make a CEL predicate: %w", err)
	}
	g.filter = prd
	g.br = br

	repo, ok := cfg.Spec.Notification.Delivery["githubRepo"].(string)
	if !ok {
		return fmt.Errorf("expected delivery config %v to have string field `githubRepo`", cfg.Spec.Notification.Delivery)
	}
	g.githubRepo = repo

	tmpl, err := template.New("issue_template").Parse(issueTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse issue body template: %w", err)
	}
	g.tmpl = tmpl

	wuRef, err := notifiers.GetSecretRef(cfg.Spec.Notification.Delivery, githubTokenSecretName)
	if err != nil {
		return fmt.Errorf("failed to get Secret ref from delivery config (%v) field %q: %w", cfg.Spec.Notification.Delivery, githubTokenSecretName, err)
	}
	wuResource, err := notifiers.FindSecretResourceName(cfg.Spec.Secrets, wuRef)
	if err != nil {
		return fmt.Errorf("failed to find Secret for ref %q: %w", wuRef, err)
	}
	wu, err := sg.GetSecret(ctx, wuResource)
	if err != nil {
		return fmt.Errorf("failed to get token secret: %w", err)
	}
	g.githubToken = wu

	return nil
}

func (g *githubissuesNotifier) SendNotification(ctx context.Context, build *cbpb.Build) error {
	if !g.filter.Apply(ctx, build) {
		log.V(2).Infof("not sending response for event (build id = %s, status = %v)", build.Id, build.Status)
		return nil
	}

	repo := GetGithubRepo(build)
	if repo == "" {
		log.Warningf("could not determine GitHub repository from build, skipping notification")
		return nil
	}
	webhookURL := fmt.Sprintf("%s/%s/issues", githubApiEndpoint, repo)

	log.Infof("sending GitHub Issue webhook for Build %q (status: %q) to url %q", build.Id, build.Status, webhookURL)

	bindings, err := g.br.Resolve(ctx, nil, build)
	if err != nil {
		log.Errorf("failed to resolve bindings :%v", err)
	}

	GetAndSetCommitterInfo(ctx, build, g, githubApiEndpoint)

	g.tmplView = &notifiers.TemplateView{
		Build:  &notifiers.BuildView{Build: build},
		Params: bindings,
	}
	logURL, err := notifiers.AddUTMParams(build.LogUrl, notifiers.HTTPMedium)
	if err != nil {
		return fmt.Errorf("failed to add UTM params: %w", err)
	}
	build.LogUrl = logURL

	payload := new(bytes.Buffer)
	var buf bytes.Buffer
	if err := g.tmpl.Execute(&buf, g.tmplView); err != nil {
		return err
	}
	err = json.NewEncoder(payload).Encode(buf)
	if err != nil {
		return fmt.Errorf("failed to encode payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(buf.String()))
	if err != nil {
		return fmt.Errorf("failed to create a new HTTP request: %w", err)
	}

	setHeaders(req, g)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warningf("got a non-OK response status %q (%d) from %q", resp.Status, resp.StatusCode, webhookURL)
	}

	log.V(2).Infoln("send create issue HTTP request successfully")

	// If the issue is created, close it by default, unless disabled
	if val, ok := notifiers.GetEnv(fmt.Sprintf("DISABLE_AUTO_CLOSE__%s", repo)); (!ok || val != "true") && resp.StatusCode == http.StatusCreated {
		var data map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			log.Warningf("failed to decode JSON response: %v", err)
		}
		if data["state"] != nil && data["state"].(string) == "open" {
			issueURL := data["url"].(string)
			req, err := http.NewRequest(http.MethodPatch, issueURL, strings.NewReader(`{"state": "closed"}`))
			if err != nil {
				log.Warningf("failed to create a new HTTP request: %v", err)
			}
			setHeaders(req, g)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("failed to make HTTP request: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				log.Warningf("got a non-OK response status %q (%d) from %q", resp.Status, resp.StatusCode, webhookURL)
			}

			log.V(2).Infoln("send close issue HTTP request successfully")
		}
	}
	return nil
}

func setHeaders(req *http.Request, g *githubissuesNotifier) {
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Authorization", fmt.Sprintf("token %s", g.githubToken))
	req.Header.Set("User-Agent", "GCB-Notifier/0.1 (http)")
}

func GetAndSetCommitterInfo(ctx context.Context, build *cbpb.Build, g *githubissuesNotifier, githubApiEndpoint string) {
	err2, committer := getCommitter(ctx, build, g, githubApiEndpoint)
	if err2 != nil {
		log.Warningf("failed to get committer from commit ref :%v", err2)
	}
	build.Substitutions["GH_COMMITTER_LOGIN"] = committer
}

func getCommitter(ctx context.Context, build *cbpb.Build, g *githubissuesNotifier, githubApiEndpoint string) (error, string) {
	// Lookup committer and set it to .Build.Substitutions.GH_COMMITTER_LOGIN
	refName := build.Substitutions["REF_NAME"]
	if refName == "" {
		return fmt.Errorf("no ref name found in substitutions"), ""
	}
	webhookURL := ""
	// if tag, use /releases/tags/{tag} instead of /commits/{refName}
	if build.Substitutions["TAG_NAME"] != "" {
		webhookURL = fmt.Sprintf("%s/%s/releases/tags/%s", githubApiEndpoint, GetGithubRepo(build), refName)
	} else {
		webhookURL = fmt.Sprintf("%s/%s/commits/%s", githubApiEndpoint, GetGithubRepo(build), refName)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webhookURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create a new HTTP request: %w", err), ""
	}
	setHeaders(req, g)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make HTTP request: %w", err), ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("got a non-OK response status %q (%d) from %q", resp.Status, resp.StatusCode, webhookURL), ""
	}
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("failed to decode JSON response: %w", err), ""
	}
	// Use author field from GitHub to avoid case where committer is "web-flow" which is assigned whenever someone edits on github.com
	if data != nil {
		if data["author"] != nil && data["author"].(map[string]interface{})["type"].(string) == "User" {
			return nil, data["author"].(map[string]interface{})["login"].(string)
		} else if data["commit"] != nil {
			if data["commit"].(map[string]interface{})["committer"] != nil {
				return nil, data["commit"].(map[string]interface{})["committer"].(map[string]interface{})["name"].(string)
			} else if data["commit"].(map[string]interface{})["author"] != nil {
				return nil, data["commit"].(map[string]interface{})["author"].(map[string]interface{})["name"].(string)
			}
		}
	}
	return nil, ""
}

func GetGithubRepo(build *cbpb.Build) string {
	if build.Substitutions != nil && build.Substitutions["REPO_FULL_NAME"] != "" {
		// return repo full name if it's available
		// e.g. "GoogleCloudPlatform/cloud-build-notifiers"
		return build.Substitutions["REPO_FULL_NAME"]
	}
	return ""
}
