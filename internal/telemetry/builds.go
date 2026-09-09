package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zyx1121/kitbash/internal/problem"
)

// The build registry of spec/kitbashd-api.yaml. kitbashd records who built
// which image from which commit, and copies one between two members' stores on
// request, which is what stops an /org Package from being built again by every
// member, see PLAN.md section 2.2.
const (
	BuildsPath = "/kitbash/v1/builds"
	ImagesPath = "/kitbash/v1/images"
)

// Build is one build record, as builds_record sends it and builds_list answers
// it. The builder is not sent: kitbashd records the peer, the same rule every
// other write on this socket follows.
type Build struct {
	Path    string `json:"path"`
	Commit  string `json:"commit"`
	Digest  string `json:"digest"`
	Builder string `json:"builder,omitempty"`
	BuiltAt string `json:"builtAt,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// buildList is the object form of a build list, the shape kitbashd answers.
type buildList struct {
	Builds []Build `json:"builds"`
}

// fetchRequest is the images_fetch body: which Package the image belongs to
// and which member kitbashd should ask for it.
type fetchRequest struct {
	Path string `json:"path"`
	From string `json:"from,omitempty"`
}

// FetchResult is what a fetch answers: the image the caller now has, how big
// it is and who it came from. The size is absent when the runtime did not say,
// which is not the same as an image of no bytes.
type FetchResult struct {
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes,omitempty"`
	From   string `json:"from"`
}

// RecordBuild tells kitbashd what this member built. It is best effort for the
// caller: a build that is not recorded is a build the next member repeats,
// which is what happened before this record existed.
func (c *Client) RecordBuild(ctx context.Context, b Build) *problem.Problem {
	if c == nil {
		return problem.Internal(b.Path, "telemetry is not configured for this session", NotRunningFix)
	}
	body, err := json.Marshal(b)
	if err != nil {
		return problem.Internal(b.Path, err.Error(), "")
	}
	status, payload, prob := c.send(ctx, http.MethodPost, BuildsPath, body, b.Path)
	if prob != nil {
		return prob
	}
	if status >= 300 {
		return c.failure(b.Path, statusText(status), payload)
	}
	return nil
}

// Builds is what kitbashd knows about one Package path, newest first. An empty
// commit and digest ask for every build of that path.
func (c *Client) Builds(ctx context.Context, path, commit, digest string) ([]Build, *problem.Problem) {
	if c == nil {
		return nil, problem.Internal(path, "telemetry is not configured for this session", NotRunningFix)
	}
	query := url.Values{}
	query.Set("path", path)
	if commit != "" {
		query.Set("commit", commit)
	}
	if digest != "" {
		query.Set("digest", digest)
	}
	status, payload, prob := c.send(ctx, http.MethodGet, BuildsPath+"?"+query.Encode(), nil, path)
	if prob != nil {
		return nil, prob
	}
	if status >= 300 {
		return nil, c.failure(path, statusText(status), payload)
	}
	var wrapped buildList
	if err := json.Unmarshal(payload, &wrapped); err != nil {
		return nil, problem.Internal(path,
			fmt.Sprintf("%s answered with a body that is not a build list: %v", BuildsPath, err), "")
	}
	return wrapped.Builds, nil
}

// FetchImage asks kitbashd to copy one image into this member's store from the
// member who built it. It is the one call of this client that waits on the
// container runtime moving hundreds of megabytes, so it goes over the same
// patient connection a Process start does.
func (c *Client) FetchImage(ctx context.Context, digest, path, from string) (*FetchResult, *problem.Problem) {
	if c == nil {
		return nil, problem.Internal(path, "telemetry is not configured for this session", NotRunningFix)
	}
	body, err := json.Marshal(fetchRequest{Path: path, From: from})
	if err != nil {
		return nil, problem.Internal(path, err.Error(), "")
	}
	// The digest reaches the daemon as one path segment. It is escaped rather
	// than trusted to be hexadecimal: this client is given the digest by a
	// caller, and a segment with a slash in it would be another path.
	fetch := ImagesPath + "/" + url.PathEscape(digest) + "/fetch"
	status, payload, prob := c.sendWith(ctx, c.supervisor, http.MethodPost, fetch, body, path)
	if prob != nil {
		return nil, prob
	}
	if status >= 300 {
		return nil, c.failure(path, statusText(status), payload)
	}
	var result FetchResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, problem.Internal(path,
			fmt.Sprintf("%s answered with a body that is not a fetch: %v", fetch, err), "")
	}
	if result.Digest != digest {
		return nil, problem.Internal(path,
			fmt.Sprintf("%s answered with the image %s", fetch, result.Digest), "")
	}
	return &result, nil
}
