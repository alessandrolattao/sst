package journal

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

// jsonOptions are the encoding/json/v2 options every state read and write
// goes through.
//
// Pulumi reads the file back with encoding/json, so these keep what v1
// tolerates: strings that are not valid UTF-8 (v1 replaces the bad bytes, v2
// would refuse the whole state), duplicate keys, and keys whose case differs
// from the struct tag. Deterministic sorts map keys as v1 always did, which is
// what lets SST skip the upload of a state that has not changed, since it
// compares a hash of the bytes.
//
// omitempty keeps v1's meaning, which leaves out false and 0 too. Measured on
// a state of 11,000 resources, v2's own meaning writes them all and makes the
// state 2.9% larger, which SST then pays again at every push. v1's makes it
// the size Pulumi writes and the quickest to encode: 360 ms (median of 9),
// against 385 for encoding/json and 377 for v2 with its own omitempty.
var jsonOptions = jsonv2.JoinOptions(
	jsonv2.Deterministic(true),
	jsonv2.MatchCaseInsensitiveNames(true),
	jsontext.AllowInvalidUTF8(true),
	jsontext.AllowDuplicateNames(true),
	json.OmitEmptyWithLegacySemantics(true),
)

// checkpoint is a VersionedCheckpoint's body with the deployment kept as raw
// bytes, so the stack name and its config go back to the file exactly as they
// were read, and only `latest` is ever rewritten.
type checkpoint struct {
	Stack  jsontext.Value `json:"stack"`
	Config jsontext.Value `json:"config,omitempty"`
	Latest jsontext.Value `json:"latest,omitempty"`
}

// state is the checkpoint file SST pulled before the update started.
type state struct {
	version  int
	features []string
	body     checkpoint
	// base is the deployment the update starts from. Pulumi gets the same
	// bytes from the export endpoint, so the indices its journal entries
	// carry point into this resource list.
	base *apitype.DeploymentV3
}

func readState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var versioned apitype.VersionedCheckpoint
	if err := jsonv2.Unmarshal(data, &versioned, jsonOptions); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	result := &state{
		version:  versioned.Version,
		features: versioned.Features,
		base:     &apitype.DeploymentV3{},
	}
	if err := jsonv2.Unmarshal(versioned.Checkpoint, &result.body, jsonOptions); err != nil {
		return nil, fmt.Errorf("reading the checkpoint in %s: %w", path, err)
	}
	if isNull(result.body.Latest) {
		// A stage that `pulumi stack init` just created has no deployment yet.
		result.body.Latest = nil
		return result, nil
	}
	if err := jsonv2.Unmarshal(result.body.Latest, result.base, jsonOptions); err != nil {
		return nil, fmt.Errorf("reading the deployment in %s: %w", path, err)
	}
	return result, nil
}

// export is the body of the export endpoint: the deployment as it was read,
// untouched, so it decodes to exactly the base the journal applies to.
func (s *state) export() apitype.UntypedDeployment {
	version := s.version
	if version == 0 {
		version = apitype.DeploymentSchemaVersionCurrent
	}
	deployment := s.body.Latest
	if deployment == nil {
		deployment = jsontext.Value("{}")
	}
	return apitype.UntypedDeployment{
		Version:    version,
		Features:   s.features,
		Deployment: json.RawMessage(deployment),
	}
}

// encode writes the checkpoint file for a deployment into buf: the stack and
// the config as they were read, and the new deployment as `latest`.
func (s *state) encode(buf *bytes.Buffer, deployment apitype.TypedDeployment) error {
	buf.Reset()
	fmt.Fprintf(buf, `{"version":%d,`, deployment.Version)
	if len(deployment.Features) > 0 {
		buf.WriteString(`"features":`)
		if err := jsonv2.MarshalWrite(buf, deployment.Features, jsonOptions); err != nil {
			return err
		}
		buf.WriteByte(',')
	}
	buf.WriteString(`"checkpoint":{"stack":`)
	buf.Write(s.body.Stack)
	if len(s.body.Config) > 0 && !isNull(s.body.Config) {
		buf.WriteString(`,"config":`)
		buf.Write(s.body.Config)
	}
	buf.WriteString(`,"latest":`)
	if err := jsonv2.MarshalWrite(buf, deployment.Deployment, jsonOptions); err != nil {
		return err
	}
	buf.WriteString("}}\n")
	return nil
}

// writeFile replaces path with data in one step. SST reads the file to push it
// while the update runs, and a rename means it only ever sees a whole one.
func writeFile(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(tmp.Name()))
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func isNull(v jsontext.Value) bool {
	return len(v) == 0 || string(bytes.TrimSpace(v)) == "null"
}
