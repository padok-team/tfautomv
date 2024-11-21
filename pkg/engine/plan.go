package engine

import (
	"fmt"
	"slices"
	"sort"

	"github.com/busser/tfautomv/pkg/engine/flatmap"
	tfjson "github.com/hashicorp/terraform-json"
)

// A Plan represents a Terraform plan. It contains the resources Terraform plans
// to create and the resources Terraform plans to delete.
type Plan struct {
	// The resources Terraform plans to create.
	ToCreate []Resource
	// The resources Terraform plans to delete.
	ToDelete []Resource
}

// SummarizeJSONPlan takes the JSON representation of a Terraform plan, as
// returned by the Terraform CLI, and condenses it into a Plan containing all
// the information the tfautomv engine needs.
//
// The moduleID argument can be any string, but must be unique for each Plan
// passed to the engine. Typically, it is the path to the module's directory.
func SummarizeJSONPlan(moduleID string, jsonPlan *tfjson.Plan) (Plan, error) {
	var planToCreate, planToDelete []Resource
	for _, rc := range jsonPlan.ResourceChanges {
		isCreated := slices.Contains(rc.Change.Actions, tfjson.ActionCreate)
		isDestroyed := slices.Contains(rc.Change.Actions, tfjson.ActionDelete)

		// Only act on resources that are created XOR destroyed but not on resources that are replaced
		isReplaced := isCreated && isDestroyed

		if (!isCreated && !isDestroyed) || isReplaced {
			continue
		}

		if isCreated {
			attributes, err := flatmap.Flatten(rc.Change.After)
			if err != nil {
				return Plan{}, fmt.Errorf("failed to flatten attributes of %s: %w", rc.Address, err)
			}

			r := Resource{
				ModuleID:   moduleID,
				Type:       rc.Type,
				Address:    rc.Address,
				Attributes: attributes,
			}

			planToCreate = append(planToCreate, r)
		}

		if isDestroyed {
			attributes, err := flatmap.Flatten(rc.Change.Before)
			if err != nil {
				return Plan{}, fmt.Errorf("failed to flatten attributes of %s: %w", rc.Address, err)
			}

			r := Resource{
				ModuleID:   moduleID,
				Type:       rc.Type,
				Address:    rc.Address,
				Attributes: attributes,
			}

			planToDelete = append(planToDelete, r)
		}
	}

	return Plan{
		ToCreate: planToCreate,
		ToDelete: planToDelete,
	}, nil
}

// MergePlans merges the given plans into a single plan. This works because the
// engine only cares about the resources Terraform plans to create and the
// resources Terraform plans to delete. The module the resource is from is part
// of the resource itself, not part of the plan.
func MergePlans(plans []Plan) Plan {
	var merged Plan
	for _, p := range plans {
		merged.ToCreate = append(merged.ToCreate, p.ToCreate...)
		merged.ToDelete = append(merged.ToDelete, p.ToDelete...)
	}
	return merged
}

// CompareAll compares each resource Terraform plans to create to each
// resource Terraform plans to delete of the same type. For each resource pair,
// it returns a ResourceComparison containing the result of the comparison.
//
// By default, the comparison checks whether the resources' attributes are
// equal. This behavior can be tweeked by passing in engine rules that allow
// certain differences to be ignored.
func CompareAll(plan Plan, rules []Rule) []ResourceComparison {
	// First, group resources by type and the action Terraform plans to take.
	createByType := make(map[string][]Resource)
	deleteByType := make(map[string][]Resource)

	for _, r := range plan.ToCreate {
		createByType[r.Type] = append(createByType[r.Type], r)
	}
	for _, r := range plan.ToDelete {
		deleteByType[r.Type] = append(deleteByType[r.Type], r)
	}

	// Then, compare each resource Terraform plans to create to all resources
	// Terraform plans to delete of the same type.
	var comparisons []ResourceComparison
	for t := range createByType {
		for _, c := range createByType[t] {
			for _, d := range deleteByType[t] {
				if c.ID() == d.ID() {
					// The resources are the same, so there's nothing to compare.
					continue
				}

				comparison := CompareResources(c, d, rules)
				comparisons = append(comparisons, comparison)
			}
		}
	}

	// Finally, sort the comparisons so that the result is deterministic.
	sortComparisons(comparisons)

	return comparisons
}

func sortComparisons(comparisons []ResourceComparison) {
	sort.Slice(comparisons, func(i, j int) bool {
		a, b := comparisons[i], comparisons[j]

		// The goal here is to group comparisons that are for the same resource
		// together.

		switch {
		case a.ToCreate.ModuleID != b.ToCreate.ModuleID:
			return a.ToCreate.ModuleID < b.ToCreate.ModuleID

		case a.ToCreate.Address != b.ToCreate.Address:
			return a.ToCreate.Address < b.ToCreate.Address

		case a.ToDelete.ModuleID != b.ToDelete.ModuleID:
			return a.ToDelete.ModuleID < b.ToDelete.ModuleID

		case a.ToDelete.Address != b.ToDelete.Address:
			return a.ToDelete.Address < b.ToDelete.Address

		default:
			return false // a == b so it doesn't matter what we return here
		}
	})
}
