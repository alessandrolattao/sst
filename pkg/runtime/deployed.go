package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
)

// A deploy builds and zips every function, then lets Pulumi find out that most
// of the zips are the ones it already uploaded. A runtime that can tell what a
// build would produce without running it implements Signer, and a function
// whose signature is the one recorded in the state at its last deploy is not
// built at all: its code object keeps the zip it has.

// Signer is implemented by a runtime that can sign a build before running it.
// Two builds with the same signature must produce the same files.
type Signer interface {
	// Sign returns the signature of the build the input asks for and the
	// handler that build reports. An empty signature means the runtime cannot
	// vouch for this build, and it is built as usual.
	Sign(ctx context.Context, input *BuildInput) (signature string, handler string, err error)
}

// deployedCode is what the state says about a function's last deployed code.
type deployedCode struct {
	// signature of the build that produced the zip
	signature string
	// key of the zip in the asset bucket
	key string
	// path the zip was read from, which is where the provider looks for it
	path string
}

const (
	functionType   = "sst:aws:Function"
	codeObjectType = "aws:s3/bucketObjectv2:BucketObjectv2"
)

// LoadDeployed reads, from the state file a deploy starts from, the signature
// and the code object of every function. It has to run before Pulumi does,
// since Pulumi rewrites that file as it goes. Any function it cannot read with
// certainty is left out, and so gets built.
func (c *Collection) LoadDeployed(statePath string) {
	deployed, err := readDeployed(statePath)
	if err != nil {
		slog.Info("no deployed signatures", "err", err)
	}
	c.deployedMut.Lock()
	c.deployed = deployed
	c.deployedMut.Unlock()
}

func readDeployed(statePath string) (map[string]deployedCode, error) {
	file, err := os.Open(statePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var untyped apitype.VersionedCheckpoint
	if err := json.NewDecoder(file).Decode(&untyped); err != nil {
		return nil, err
	}
	var checkpoint apitype.CheckpointV3
	if err := json.Unmarshal(untyped.Checkpoint, &checkpoint); err != nil {
		return nil, err
	}
	if checkpoint.Latest == nil {
		return nil, nil
	}

	type recorded struct {
		urn       string
		signature string
		key       string
	}
	functions := map[string]recorded{}
	// a name two functions share cannot say which one a build is for
	ambiguous := map[string]bool{}
	for _, res := range checkpoint.Latest.Resources {
		if res.Delete || string(res.Type) != functionType {
			continue
		}
		name := res.URN.Name()
		if _, seen := functions[name]; seen {
			ambiguous[name] = true
			continue
		}
		code, _ := res.Outputs["_code"].(map[string]any)
		signature, _ := code["signature"].(string)
		key, _ := code["key"].(string)
		if signature == "" || key == "" {
			continue
		}
		functions[name] = recorded{urn: string(res.URN), signature: signature, key: key}
	}

	result := map[string]deployedCode{}
	for _, res := range checkpoint.Latest.Resources {
		if res.Delete || string(res.Type) != codeObjectType {
			continue
		}
		name, ok := strings.CutSuffix(res.URN.Name(), "Code")
		if !ok || ambiguous[name] {
			continue
		}
		fn, ok := functions[name]
		if !ok || string(res.Parent) != fn.urn {
			continue
		}
		// The signature is recorded once the function runs the zip, the object
		// as soon as it is uploaded. When a deploy fails between the two they
		// disagree, and the signature no longer describes what is deployed.
		key, _ := res.Inputs["key"].(string)
		if key != fn.key {
			continue
		}
		source, _ := res.Inputs["source"].(map[string]any)
		path, _ := source["path"].(string)
		if path == "" {
			continue
		}
		result[name] = deployedCode{signature: fn.signature, key: key, path: path}
	}
	return result, nil
}

// sign returns the signature of the build the input asks for, or "" when the
// build has to run: the caller did not ask, the runtime cannot sign, or the
// build puts in the zip something the signature does not cover.
func (c *Collection) sign(ctx context.Context, input *BuildInput) (signature string, handler string) {
	if input.Code == "" || input.Dev || input.IsContainer || input.Bundle != "" || len(input.CopyFiles) > 0 {
		return "", ""
	}
	r, ok := c.Runtime(input.Runtime)
	if !ok {
		return "", ""
	}
	signer, ok := r.(Signer)
	if !ok {
		return "", ""
	}
	built, handler, err := signer.Sign(ctx, input)
	if err != nil {
		slog.Info("could not sign the build", "functionID", input.FunctionID, "err", err)
		return "", ""
	}
	if built == "" {
		return "", ""
	}
	// what Build adds to the runtime's files: the linked resources, encrypted
	links, err := json.Marshal(input.Links)
	if err != nil {
		return "", ""
	}
	hash := sha256.New()
	for _, part := range [][]byte{
		[]byte(built),
		[]byte(input.Runtime),
		[]byte(handler),
		input.Properties,
		[]byte(input.EncryptionKey),
		links,
	} {
		hash.Write([]byte{byte(len(part) >> 24), byte(len(part) >> 16), byte(len(part) >> 8), byte(len(part))})
		hash.Write(part)
	}
	return hex.EncodeToString(hash.Sum(nil)), handler
}

// deployedKey returns the key of the zip the function already runs when that
// zip came from a build with this signature, or "" when it has to be built.
func (c *Collection) deployedKey(input *BuildInput, signature string) string {
	if signature == "" {
		return ""
	}
	c.deployedMut.RLock()
	code, ok := c.deployed[input.FunctionID]
	c.deployedMut.RUnlock()
	// The provider reads the zip from the path the state recorded. The caller
	// leaves a stand-in at input.Code, so the two have to be the same file.
	if !ok || code.signature != signature || code.path != input.Code {
		return ""
	}
	return code.key
}
