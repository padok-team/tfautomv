package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hashicorp/go-version"
	flag "github.com/spf13/pflag"

	"github.com/busser/tfautomv/pkg/engine"
	"github.com/busser/tfautomv/pkg/engine/rules"
	"github.com/busser/tfautomv/pkg/pretty"
	"github.com/busser/tfautomv/pkg/terraform"
)

func main() {
	if err := run(); err != nil {
		os.Stderr.WriteString(pretty.Error(err) + "\n")
		os.Exit(1)
	}
}

//go:embed VERSION
var tfautomvVersion string

func run() error {
	parseFlags()

	workdirs := flag.Args()
	if len(workdirs) == 0 {
		workdirs = []string{"."}
	}

	if noColor {
		pretty.DisableColors()
	}

	if printVersion {
		fmt.Println(tfautomvVersion)
		return nil
	}

	ctx := context.TODO()

	/*
	 * Step 0: Smoke tests
	 *
	 * Detect any obvious issues with the user's configuration.
	 */

	if outputFormat != "auto" && outputFormat != "blocks" && outputFormat != "commands" {
		return fmt.Errorf("unknown output format %q", outputFormat)
	}

	if outputFormat == "blocks" && len(flag.Args()) > 1 {
		return fmt.Errorf("blocks output format is not supported for multiple modules")
	}

	tfVersion, err := terraform.GetVersion(ctx, terraform.WithTerraformBin(terraformBin))
	if err != nil {
		return fmt.Errorf("failed to get Terraform version: %w", err)
	}

	movedBlocksSupported := tfVersion.GreaterThanOrEqual(version.Must(version.NewSemver("1.1.0")))
	if outputFormat == "blocks" && !movedBlocksSupported {
		return fmt.Errorf("Terraform version %s does not support moved blocks", tfVersion)
	}

	crossModuleMovesSupported := tfVersion.GreaterThanOrEqual(version.Must(version.NewSemver("0.14.0")))
	if len(workdirs) > 1 && !crossModuleMovesSupported {
		return fmt.Errorf("Terraform version %s does not support moves across modules", tfVersion)
	}

	/*
	 * Step 1: Parse user-provided rules
	 *
	 * Best to parse the rules early on, so that any syntax errors are caught
	 * before we start running Terraform commands.
	 */

	var userRules []engine.Rule
	for _, raw := range ignoreRules {
		rule, err := rules.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid rule passed with -ignore flag %q: %w", raw, err)
		}

		userRules = append(userRules, rule)
	}

	/*
	 * Step 2: Obtain Terraform plan if no plan file paths given
	 *
	 * Run `terraform plan` for each working directory provided by the user.
	 * If plan file paths given read it in.
	 */

	terraformOptions := []terraform.Option{
		terraform.WithTerraformBin(terraformBin),
		terraform.WithSkipInit(skipInit),
		terraform.WithSkipRefresh(skipRefresh),
	}

	plans := make([]engine.Plan, 0)
	if planPaths == nil {
		plans, err = getPlans(ctx, workdirs, terraformOptions)
		if err != nil {
			return err
		}

	} else {
		for _, planPath := range planPaths {
			jsonPlan, err := terraform.GetPlanFromPath(planPath)
			if err != nil {
				return err
			}
			planPathSplit := strings.Split(planPath, "/")
			moduleID := strings.Join(planPathSplit[:len(planPathSplit)-1], "/")
			plan, err := engine.SummarizeJSONPlan(moduleID, jsonPlan)
			if err != nil {
				return err
			}
			plans = append(plans, plan)
		}
	}

	/*
	 * Step 3: Use the tfautomv engine to determine moves to make
	 *
	 * We merge the plans from each working directory into a single plan
	 * containing all the resources. This doesn't make a difference to the
	 * engine.
	 */

	mergedPlan := engine.MergePlans(plans)
	comparisons := engine.CompareAll(mergedPlan, userRules)
	moves := engine.DetermineMoves(comparisons)

	/*
	 * Step 4: Print a human-readable summary for the user
	 *
	 * This summary allows the user to gain insight into what tfautomv has
	 * done, and why. This information allows the user to make an informed
	 * decision about what to do next.
	 */

	summarizer := pretty.NewSummarizer(moves, comparisons, verbosity)
	summary := summarizer.Summary()

	os.Stderr.WriteString("\n" + summary + "\n\n")

	/*
	 * Step 5: Write the moves found by the engine.
	 *
	 * Depending on the output format chosen by the user, we either write
	 * the moves to a file, print them to standard output, or a combination
	 * of both.
	 */

	terraformMoves := engineMovesToTerraformMoves(moves)
	sameModule, differentModule := categorizeMoves(terraformMoves)

	switch outputFormat {
	case "auto":
		switch {
		case movedBlocksSupported:
			if err := writeMovedBlocks(sameModule); err != nil {
				return err
			}
			if err := writeMoveCommands(differentModule, terraformOptions...); err != nil {
				return err
			}
		case !movedBlocksSupported:
			if err := writeMoveCommands(terraformMoves, terraformOptions...); err != nil {
				return err
			}
		}
	case "blocks":
		if err := writeMovedBlocks(sameModule); err != nil {
			return err
		}
	case "commands":
		if err := writeMoveCommands(terraformMoves, terraformOptions...); err != nil {
			return err
		}
	default:
		// This should have been caught by the smoke tests.
		return fmt.Errorf("unknown output format %q", outputFormat)
	}

	return nil
}

// Flags
var (
	ignoreRules  []string
	noColor      bool
	outputFormat string
	printVersion bool
	skipInit     bool
	skipRefresh  bool
	terraformBin string
	verbosity    int
	planPaths    []string
)

func parseFlags() {
	flag.StringSliceVar(&ignoreRules, "ignore", nil, "ignore differences based on a `rule`")
	flag.BoolVar(&noColor, "no-color", false, "disable color in output")
	flag.StringVarP(&outputFormat, "output", "o", "auto", "output `format` of moves (\"auto\", \"blocks\" or \"commands\")")
	flag.BoolVarP(&printVersion, "version", "V", false, "print version and exit")
	flag.BoolVarP(&skipInit, "skip-init", "s", false, "skip running terraform init")
	flag.BoolVarP(&skipRefresh, "skip-refresh", "S", false, "skip running terraform refresh")
	flag.StringVar(&terraformBin, "terraform-bin", "terraform", "terraform binary to use")
	flag.CountVarP(&verbosity, "verbosity", "v", "increase verbosity (can be specified multiple times)")
	flag.StringSliceVar(&planPaths, "plan-path", nil, "path to a plan file to use")

	flag.Parse()
}

func engineMovesToTerraformMoves(moves []engine.Move) []terraform.Move {
	var terraformMoves []terraform.Move

	for _, m := range moves {
		terraformMoves = append(terraformMoves, terraform.Move{
			FromWorkdir: m.SourceModule,
			ToWorkdir:   m.DestinationModule,
			FromAddress: m.SourceAddress,
			ToAddress:   m.DestinationAddress,
		})
	}

	return terraformMoves
}

func getPlans(ctx context.Context, workdirs []string, options []terraform.Option) ([]engine.Plan, error) {
	type result struct {
		plan engine.Plan
		err  error
	}
	results := make([]result, len(workdirs))

	getPlan := func(i int) {
		workdir := workdirs[i]

		os.Stderr.WriteString(pretty.Colorf("getting Terraform plan for %s...", (*pretty.Summarizer).StyledModule(nil, workdir)) + "\n")

		workdirOptions := append(
			[]terraform.Option{terraform.WithWorkdir(workdir)},
			options...,
		)

		jsonPlan, err := terraform.GetPlan(ctx, workdirOptions...)
		if err != nil {
			results[i].err = fmt.Errorf("failed to get plan for workdir %q: %w", workdir, err)
			return
		}

		plan, err := engine.SummarizeJSONPlan(workdirs[i], jsonPlan)
		if err != nil {
			results[i].err = fmt.Errorf("failed to summarize plan for workdir %q: %w", workdir, err)
			return
		}

		results[i].plan = plan
	}

	var wg sync.WaitGroup
	for i := range workdirs {
		wg.Add(1)
		go func(i int) {
			getPlan(i)
			wg.Done()
		}(i)
	}

	wg.Wait()

	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	var plans []engine.Plan
	for _, r := range results {
		plans = append(plans, r.plan)
	}

	return plans, nil
}

func categorizeMoves(moves []terraform.Move) (sameWorkdir, differentWorkdir []terraform.Move) {
	for _, m := range moves {
		if m.FromWorkdir == m.ToWorkdir {
			sameWorkdir = append(sameWorkdir, m)
		} else {
			differentWorkdir = append(differentWorkdir, m)
		}
	}

	return sameWorkdir, differentWorkdir
}

func writeMovedBlocks(moves []terraform.Move) error {
	if len(moves) == 0 {
		return nil
	}

	movesByWorkdir := make(map[string][]terraform.Move)
	for _, m := range moves {
		movesByWorkdir[m.FromWorkdir] = append(movesByWorkdir[m.FromWorkdir], m)
	}

	for workdir, moves := range movesByWorkdir {
		movesFilePath := filepath.Join(workdir, "moves.tf")
		movesFile, err := os.OpenFile(movesFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("failed to open %q: %w", movesFilePath, err)
		}

		err = terraform.WriteMovedBlocks(movesFile, moves)
		if err != nil {
			return fmt.Errorf("failed to write moved blocks: %w", err)
		}

		os.Stderr.WriteString(pretty.Colorf("%s written to [bold][green]%s", pretty.StyledNumMoves(len(moves)), movesFilePath))
		os.Stderr.WriteString("\n")
	}

	return nil
}

func writeMoveCommands(moves []terraform.Move, options ...terraform.Option) error {
	if len(moves) == 0 {
		return nil
	}

	err := terraform.WriteMoveCommands(os.Stdout, moves, options...)
	if err != nil {
		return fmt.Errorf("failed to write move commands: %w", err)
	}

	os.Stderr.WriteString(pretty.Colorf("%s written to [bold][green]standard output", pretty.StyledNumMoves(len(moves))))
	os.Stderr.WriteString("\n")

	return nil
}
