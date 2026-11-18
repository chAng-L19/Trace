package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/mcp"
	"trace/internal/web"
)

func serveCommand(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:8765", "local listen address")
	configPath := flags.String("config", "", "service configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: trace serve [--addr 127.0.0.1:8765] [--config FILE]")
	}
	if err := web.ParseAddress(*addr); err != nil {
		return err
	}
	dataDir := os.Getenv("TRACE_DATA_DIR")
	if dataDir == "" {
		dataDir = "state"
	}
	if *configPath == "" {
		*configPath = serviceConfigPath()
	}
	store, err := agent.NewStore(dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	result, err := loadServiceConfig(*configPath, dataDir)
	if err != nil {
		return err
	}
	if result.IsFirstBoot {
		log.Println("首次启动：将在首次配置保存时创建配置文件")
	}
	config := result.Config
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	build := func(ctx context.Context, c web.Config) (*agent.Agent, error) {
		return buildWebAgent(ctx, store, c)
	}
	runner, err := build(ctx, config)
	if err != nil {
		return err
	}
	if err = runner.RecoverExecResources(); err != nil {
		return errors.Join(err, runner.CloseMCP())
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return errors.Join(err, runner.CloseMCP())
	}
	app, err := web.New(runner, web.Options{ConfigPath: *configPath, InitialConfig: config, BuildAgent: build, Host: listener.Addr().String()})
	if err != nil {
		listener.Close()
		return errors.Join(err, runner.CloseMCP())
	}
	defer app.Close()
	server := &http.Server{Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("Trace WebUI: http://%s\n", listener.Addr())
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// Stop runtime lifetimes before closing event streams; disconnect never controls a run.
	if err = app.Close(); err != nil {
		_ = server.Close()
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func buildWebAgent(ctx context.Context, store *agent.Store, c web.Config) (*agent.Agent, error) {
	return buildConfiguredAgent(ctx, store, c, true, true)
}

// Both entrypoints use the same Agent and registry construction. Metadata-only
// CLI commands do not connect tools; env-only CLI resumes retain Run context
// unless a context setting was explicitly supplied.
func buildConfiguredAgent(ctx context.Context, store *agent.Store, c web.Config, connectTools, overrideContext bool) (runner *agent.Agent, buildErr error) {
	provider, providers, roles, err := c.ModelConfiguration()
	if err != nil {
		return nil, err
	}
	runner, buildErr = agent.New(store, provider, c.PromptPath)
	if buildErr != nil {
		return nil, buildErr
	}
	if buildErr = runner.ConfigureModels(providers, roles); buildErr != nil {
		return nil, buildErr
	}
	if buildErr = runner.ConfigureSearchSources(c.SearchSources); buildErr != nil {
		return nil, buildErr
	}
	defer func() {
		if buildErr != nil {
			cleanupErr := runner.CloseMCP()
			var connection *mcp.ConnectionError
			if cleanupErr != nil && errors.As(buildErr, &connection) {
				connection.Status = connection.Status.WithCleanup("candidate_cleanup", cleanupErr)
			}
			buildErr = errors.Join(buildErr, cleanupErr)
		}
	}()
	budget, err := agent.BudgetFromEnv()
	if err != nil {
		return runner, err
	}
	if err := runner.ConfigureCosts(c.Costs); err != nil {
		return runner, err
	}
	runner.ConfigureBudget(budget)
	treeBudget, err := agent.TreeBudgetFromEnv()
	if err != nil {
		return runner, err
	}
	if err = runner.ConfigureTreeBudget(treeBudget); err != nil {
		return runner, err
	}
	runner.SetSuccessContains(os.Getenv("TRACE_SUCCESS_CONTAINS"))
	runner.SetRequireVerifiedCompletion(os.Getenv("TRACE_REQUIRE_VERIFIED_COMPLETION") == "true")
	if overrideContext {
		if err = runner.ConfigureContext(c.Context); err != nil {
			return runner, err
		}
	}
	if err = runner.ConfigureSkills(c.SkillPaths, c.DisabledSkills...); err != nil {
		return runner, err
	}
	if connectTools {
		if err = registerConfiguredTools(ctx, runner, c); err != nil {
			return runner, err
		}
	}
	return runner, nil
}

func initialWebConfig() (web.Config, error) {
	c := web.Config{Provider: web.ProviderConfig{URL: os.Getenv("TRACE_PROVIDER_URL"), APIKey: os.Getenv("TRACE_PROVIDER_KEY"), Model: os.Getenv("TRACE_MODEL"), ImageInput: os.Getenv("TRACE_IMAGE_INPUT") == "true"}, PromptPath: os.Getenv("TRACE_PROMPT_FILE")}
	if c.PromptPath == "" {
		candidates := []string{filepath.Join("prompts", "agent.md")}
		if executable, err := os.Executable(); err == nil {
			candidates = append([]string{filepath.Join(filepath.Dir(executable), "prompts", "agent.md")}, candidates...)
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				c.PromptPath = candidate
				break
			}
		}
	}
	var err error
	c.Context, err = agent.ContextFromEnv()
	if err != nil {
		return c, err
	}
	if paths := os.Getenv("TRACE_SKILL_PATHS"); paths != "" {
		c.SkillPaths = filepath.SplitList(paths)
	}
	for _, item := range []struct {
		inline, path string
		out          any
	}{{"TRACE_MCP_SERVERS", "TRACE_MCP_CONFIG", &c.MCP}, {"TRACE_LOCAL_TOOLS", "TRACE_LOCAL_TOOLS_CONFIG", &c.LocalTools}} {
		data := []byte(os.Getenv(item.inline))
		path := os.Getenv(item.path)
		if path != "" {
			if len(data) > 0 {
				return c, fmt.Errorf("set only one of %s or %s", item.inline, item.path)
			}
			data, err = os.ReadFile(path)
			if err != nil {
				return c, err
			}
		}
		if len(data) > 0 {
			if err = json.Unmarshal(data, item.out); err != nil {
				return c, err
			}
		}
	}
	if strings.TrimSpace(c.Provider.URL) == "" {
		c.Provider.URL = ""
	}
	return c, nil
}

func mergeWebPresets(c *web.Config, presets []localtool.Manifest) {
	custom := map[string]bool{}
	for _, m := range c.LocalTools {
		custom[m.Name] = true
	}
	for _, m := range presets {
		if !custom[m.Name] {
			c.LocalTools = append(c.LocalTools, m)
		}
	}
}
