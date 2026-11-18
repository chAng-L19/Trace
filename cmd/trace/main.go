package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"trace/internal/agent"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "trace:", err)
		os.Exit(1)
	}
}

func run(args []string) (runErr error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var registered *agent.Agent
	defer func() {
		if registered != nil {
			runErr = errors.Join(runErr, registered.CloseMCP())
		}
	}()
	if len(args) == 0 {
		printUsage()
		return nil
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printUsage()
		return nil
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if args[0] == "serve" {
		return serveCommand(args[1:])
	}
	dataDir := os.Getenv("TRACE_DATA_DIR")
	if dataDir == "" {
		dataDir = "state"
	}
	if args[0] == "store-migrate" {
		if len(args) != 2 || args[1] != "sqlite" {
			return fmt.Errorf("usage: trace store-migrate sqlite (offline; TRACE_DATA_DIR selects root)")
		}
		return agent.MigrateStoreToSQLite(dataDir)
	}
	store, err := agent.NewStore(dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	switch args[0] {
	case "store-export":
		if len(args) != 2 {
			return fmt.Errorf("usage: trace store-export NEW_DIRECTORY (offline JSON snapshots; preserve CAS/inputs separately)")
		}
		return store.ExportSQLiteSnapshots(args[1])
	case "tools", "mcp":
		if len(args) != 1 {
			return fmt.Errorf("usage: trace %s", args[0])
		}
		runner, err := configuredDirectoryAgent(ctx, store)
		if err != nil {
			return err
		}
		registered = runner
		tools := runner.Registry().Tools()
		if args[0] == "mcp" {
			external := make([]agent.RegisteredTool, 0, len(tools))
			for _, tool := range tools {
				if tool.Adapter == "mcp" || tool.Adapter == "local" {
					external = append(external, tool)
				}
			}
			tools = external
		}
		return printJSON(tools, nil)
	case "create":
		runner, err := newAgent(store)
		if err != nil {
			return err
		}
		content, attachments, err := inputArguments(store, args[1:])
		if err != nil {
			return err
		}
		return create(runner, content, attachments...)
	case "run":
		if len(args) != 2 {
			return fmt.Errorf("usage: trace run RUN_ID")
		}
		runner, err := configuredRuntimeAgent(ctx, store)
		if err != nil {
			return err
		}
		registered = runner
		snap, err := runner.Run(ctx, args[1])
		if err != nil {
			return printJSON(snap, err)
		}
		return printJSON(snap, nil)
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: trace show RUN_ID")
		}
		snap, err := store.Load(args[1])
		return printJSON(snap, err)
	case "events":
		if len(args) != 2 {
			return fmt.Errorf("usage: trace events RUN_ID")
		}
		snap, err := store.Load(args[1])
		if err != nil {
			return err
		}
		return printJSON(snap.Events, nil)
	case "list":
		runs, err := store.List()
		return printJSON(runs, err)
	case "chains":
		if len(args) == 1 {
			value, err := store.ListEvidenceChains()
			return printJSON(value, err)
		}
		if len(args) == 3 {
			value, err := store.GetEvidenceChain(args[1], args[2])
			return printJSON(value, err)
		}
		return fmt.Errorf("usage: trace chains [RUN_ID CHAIN_ID]")
	case "chain-create":
		if len(args) != 3 {
			return fmt.Errorf("usage: trace chain-create RUN_ID JSON_FILE")
		}
		data, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		var chain agent.EvidenceChain
		if err = json.Unmarshal(data, &chain); err != nil {
			return err
		}
		value, err := store.CreateEvidenceChain(args[1], chain)
		return printJSON(value, err)
	case "chain-reference":
		if len(args) != 5 {
			return fmt.Errorf("usage: trace chain-reference TARGET_RUN SOURCE_RUN CHAIN_ID CHAIN_HASH")
		}
		_, err := store.ReferenceEvidenceChain(args[1], agent.EvidenceChainReference{SourceRunID: args[2], ChainID: args[3], ChainHash: args[4]})
		return printJSON(map[string]bool{"referenced": err == nil}, err)
	case "result", "evidence":
		if len(args) != 2 {
			return fmt.Errorf("usage: trace %s RUN_ID", args[0])
		}
		if args[0] == "result" {
			value, err := store.Result(args[1])
			return printJSON(value, err)
		}
		value, err := store.EvidenceLedger(args[1])
		return printJSON(value, err)
	case "report":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: trace report RUN_ID [OUTPUT]")
		}
		report, err := store.Report(args[1])
		if err != nil {
			return err
		}
		if len(args) == 2 {
			fmt.Print(report)
			return nil
		}
		file, err := os.OpenFile(args[2], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = file.WriteString(report)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	case "message", "resume", "tool", "pause", "cancel":
		if (args[0] == "pause" || args[0] == "cancel") && len(args) != 2 {
			return fmt.Errorf("usage: trace %s RUN_ID", args[0])
		}
		if (args[0] == "message" && len(args) < 3) || (args[0] == "resume" && len(args) != 2) || (args[0] == "tool" && len(args) < 4) {
			return fmt.Errorf("usage: trace %s RUN_ID ...", args[0])
		}
		if args[0] == "tool" && args[3] == "--exec" && (len(args) < 5 || strings.TrimSpace(args[4]) == "") {
			return fmt.Errorf("usage: trace tool RUN_ID CALL_ID --exec COMMAND [ARGS...]")
		}
		if args[0] == "tool" && args[3] == "--mcp" && (len(args) != 5 || strings.TrimSpace(args[4]) == "") {
			return fmt.Errorf("usage: trace tool RUN_ID CALL_ID --mcp TOOL_ID")
		}
		if args[0] == "tool" && args[3] == "--registered" && (len(args) != 5 || strings.TrimSpace(args[4]) == "") {
			return fmt.Errorf("usage: trace tool RUN_ID CALL_ID --registered TOOL_ID")
		}
		var runner *agent.Agent
		if args[0] == "pause" || args[0] == "cancel" {
			runner, err = agent.New(store, agent.MockProvider{}, "")
		} else {
			runner, err = configuredRuntimeAgent(ctx, store)
		}
		if err != nil {
			return err
		}
		if args[0] == "pause" {
			snap, err := runner.Pause(args[1])
			return printJSON(snap, err)
		}
		if args[0] == "cancel" {
			snap, err := runner.Cancel(args[1])
			return printJSON(snap, err)
		}
		registered = runner
		if args[0] == "resume" {
			snap, err := runner.Run(ctx, args[1])
			if err != nil {
				return printJSON(snap, err)
			}
			return printJSON(snap, nil)
		}
		if args[0] == "tool" {
			if args[3] == "--registered" {
				snap, err := runner.ExecuteRegistered(ctx, args[1], args[2], args[4])
				return printJSON(snap, err)
			}
			if args[3] == "--mcp" {
				snap, err := runner.ExecuteMCP(ctx, args[1], args[2], args[4])
				return printJSON(snap, err)
			}
			if args[3] == "--exec" {
				snap, err := runner.ExecuteTool(ctx, args[1], args[2], args[4:])
				if err != nil {
					return printJSON(snap, err)
				}
				return printJSON(snap, nil)
			}
			snap, err := runner.SubmitTool(ctx, args[1], args[2], strings.TrimSpace(strings.Join(args[3:], " ")))
			if err != nil {
				return printJSON(snap, err)
			}
			return printJSON(snap, nil)
		}
		content, attachments, err := inputArguments(store, args[2:])
		if err != nil {
			return err
		}
		snap, err := runner.Message(ctx, args[1], content, attachments...)
		if err != nil {
			return printJSON(snap, err)
		}
		return printJSON(snap, nil)
	case "prompt":
		runner, err := newAgent(store)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"sha256": runner.PromptHash()}, nil)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return usageError()
	}
}

func create(runner *agent.Agent, goal string, attachments ...agent.InputAttachment) error {
	if goal == "" {
		return fmt.Errorf("usage: trace create GOAL")
	}
	var config agent.RunConfiguration
	if path := os.Getenv("TRACE_REQUIREMENTS_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		config.Requirements, err = agent.DecodeRequirements(data)
		if err != nil {
			return err
		}
	}
	snap, err := runner.CreateConfigured(goal, config, attachments...)
	return printJSON(snap.Run, err)
}

func printJSON(value any, err error) error {
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func usageError() error {
	return fmt.Errorf("unknown command; use trace help")
}

func newAgent(store *agent.Store) (*agent.Agent, error) {
	return configuredAgent(store, "")
}

func printUsage() {
	fmt.Println("Trace Agent " + version)
	fmt.Println("usage:")
	fmt.Println("  trace store-migrate sqlite       offline explicit migration of TRACE_DATA_DIR")
	fmt.Println("  trace store-export NEW_DIRECTORY offline JSON snapshot export; CAS/inputs separate")
	fmt.Println("  trace serve [--addr 127.0.0.1:8765] [--config FILE]")
	fmt.Println("  trace create GOAL [--attach FILE ...]")
	fmt.Println("  trace run RUN_ID")
	fmt.Println("  trace resume RUN_ID")
	fmt.Println("  trace message RUN_ID MESSAGE [--attach FILE ...]")
	fmt.Println("  trace tool RUN_ID CALL_ID RESULT")
	fmt.Println("  trace tool RUN_ID CALL_ID --exec COMMAND [ARGS...]")
	fmt.Println("  trace tool RUN_ID CALL_ID --mcp TOOL_ID")
	fmt.Println("  trace tool RUN_ID CALL_ID --registered TOOL_ID")
	fmt.Println("  trace tools")
	fmt.Println("  trace mcp (external local/MCP tool directory; excludes built-in tools)")
	fmt.Println("  trace pause RUN_ID")
	fmt.Println("  trace cancel RUN_ID")
	fmt.Println("  trace show RUN_ID")
	fmt.Println("  trace events RUN_ID")
	fmt.Println("  trace list")
	fmt.Println("  trace result RUN_ID")
	fmt.Println("  trace evidence RUN_ID")
	fmt.Println("  trace chains [RUN_ID CHAIN_ID]")
	fmt.Println("  trace chain-create RUN_ID JSON_FILE")
	fmt.Println("  trace chain-reference TARGET_RUN SOURCE_RUN CHAIN_ID CHAIN_HASH")
	fmt.Println("  trace report RUN_ID [OUTPUT]")
	fmt.Println("  trace prompt")
	fmt.Println("  trace version")
	fmt.Println("Budget env: TRACE_MAX_ROUNDS, TRACE_MAX_ACTIONS, TRACE_MAX_SECONDS (0=unlimited); TRACE_SUCCESS_CONTAINS applies on create")
	fmt.Println("MCP env: TRACE_MCP_CONFIG=JSON file or TRACE_MCP_SERVERS=JSON array; stable IDs from trace mcp or trace tools")
	fmt.Println("Shared configuration: TRACE_CONFIG_FILE (default TRACE_DATA_DIR/service/web-config.json); serve --config overrides this path")
	fmt.Println("Saved Provider/Context/Skill paths/MCP/local tools/prompt settings take precedence; TRACE_* configuration env initializes a missing file only")
	fmt.Println("Budget and TRACE_SUCCESS_CONTAINS remain separate run settings; existing runs retain their saved prompt")
	fmt.Println("Local tools env: TRACE_LOCAL_TOOLS_CONFIG=JSON file or TRACE_LOCAL_TOOLS=JSON array; stable IDs from trace tools")
}

func configuredAgent(store *agent.Store, promptPath string) (*agent.Agent, error) {
	result, err := cliServiceConfig()
	if err != nil {
		return nil, err
	}
	if promptPath != "" {
		result.Config.PromptPath = promptPath
	}
	return buildConfiguredAgent(context.Background(), store, result.Config, false, cliContextOverride(result))
}
