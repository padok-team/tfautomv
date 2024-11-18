package terraform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
)

// GetPlan obtains a Terraform plan from the module in the given working
// directory. It does so by running a series of Terraform commands.
func GetPlan(ctx context.Context, opts ...Option) (*tfjson.Plan, error) {
	var settings settings

	settings.apply(append(defaultOptions(), opts...))

	err := settings.validate()
	if err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}

	tf, err := tfexec.NewTerraform(settings.workdir, settings.terraformBin)
	if err != nil {
		return nil, fmt.Errorf("failed to create Terraform executor: %w", err)
	}

	if !settings.skipInit {
		err := tf.Init(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Terraform: %w", err)
		}
	}

	planFile, err := os.CreateTemp("", "tfautomv.*.plan")
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary file to store raw plan: %w", err)
	}
	defer os.Remove(planFile.Name())

	_, err = tf.Plan(ctx, tfexec.Out(planFile.Name()), tfexec.Refresh(!settings.skipRefresh))
	if err != nil {
		return nil, fmt.Errorf("failed to compute Terraform plan: %w", err)
	}

	plan, err := tf.ShowPlanFile(ctx, planFile.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to read raw Terraform plan: %w", err)
	}

	return plan, nil
}

func GetPlanFromPath(p string) (*tfjson.Plan, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}

	// Unmarshal the JSON data into a Plan struct
	var plan tfjson.Plan
	err = json.Unmarshal(data, &plan)
	if err != nil {
		return nil, err
	}

	return &plan, nil

}
