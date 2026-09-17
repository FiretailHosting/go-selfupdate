package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	defaultAPIBase = "https://api.github.com"
	apiVersion     = "2022-11-28"
)

// Release is the subset of a GitHub release this package needs.
type Release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Published  string  `json:"published_at"`
	Assets     []Asset `json:"assets"`
}

// Asset is a file attached to a release.
type Asset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// FindAsset returns the asset with this exact name.
func (r Release) FindAsset(name string) (Asset, bool) {
	for _, asset := range r.Assets {
		if asset.Name == name {
			return asset, true
		}
	}

	return Asset{}, false
}

// Latest returns the repository's latest published release.
//
// GitHub's "latest" endpoint ignores drafts and prereleases, so a release
// marked prerelease is invisible here by design.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", u.apiBase(), u.cfg.Owner, u.cfg.Repo)

	req, err := u.newRequest(ctx, url, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}

	resp, err := u.apiClient().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return Release{}, err
	}

	var release Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&release); err != nil {
		return Release{}, fmt.Errorf("cannot read the GitHub response: %w", err)
	}

	return release, nil
}

// download streams a release asset.
//
// The asset endpoint redirects to signed storage. Go's client drops the
// Authorization header on a cross-host redirect, which is what that URL wants.
func (u *Updater) download(ctx context.Context, asset Asset) (io.ReadCloser, error) {
	limit := u.maxAssetBytes()
	if asset.Size > limit {
		return nil, fmt.Errorf("release asset %q is %d bytes, over the %d byte limit", asset.Name, asset.Size, limit)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", u.apiBase(), u.cfg.Owner, u.cfg.Repo, asset.ID)

	req, err := u.newRequest(ctx, url, "application/octet-stream")
	if err != nil {
		return nil, err
	}

	resp, err := u.downloadClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot download the release asset: %w", err)
	}

	if err := checkResponse(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}

	return resp.Body, nil
}

func (u *Updater) newRequest(ctx context.Context, url, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", u.userAgent())

	if u.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.cfg.Token)
	}

	return req, nil
}

// checkResponse turns a non-2xx GitHub response into something actionable.
//
// The status codes are easy to misread: on a private repository an invalid or
// under-scoped token gives 404, not 403, which sends you checking the URL when
// the token is at fault.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub rejected the token (401): it is wrong or expired")

	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return fmt.Errorf("GitHub rate limit reached; try again later")
		}
		return fmt.Errorf("GitHub denied the request (403): the token needs read access to the repository's contents")

	case http.StatusNotFound:
		return fmt.Errorf("not found (404): check the owner and repo, that a release exists, " +
			"and that the token can read this repository if it is private")
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))

	return fmt.Errorf("GitHub returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
}
