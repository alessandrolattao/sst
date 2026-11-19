package runtime

import (
	"context"
	"net/rpc"

	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/runtime"
)

type Runtime struct {
	project *project.Project
}

func (r *Runtime) Build(input *runtime.BuildInput, output *runtime.BuildOutput) error {
	input.CfgPath = r.project.PathConfig()
	result, err := r.project.Runtime.Build(context.Background(), input)
	if err != nil {
		return err
	}
	if result.Deployed != "" {
		bus.Publish(&runtime.UnchangedEvent{
			FunctionID: input.FunctionID,
			Handler:    input.Handler,
			Signature:  result.Signature,
		})
	} else if len(result.Errors) == 0 {
		bus.Publish(&runtime.BuildEvent{
			FunctionID: input.FunctionID,
			Handler:    input.Handler,
			Duration:   result.Duration,
			Signature:  result.Signature,
			Replaces:   result.Replaces,
			Reason:     result.Reason,
		})
	}
	*output = *result
	return nil
}

func (r *Runtime) AddTarget(input *runtime.BuildInput, output *bool) error {
	bus.Publish(input)
	*output = true
	return nil
}

func Register(ctx context.Context, p *project.Project, r *rpc.Server) error {
	r.RegisterName("Runtime", &Runtime{
		project: p,
	})
	return nil
}
