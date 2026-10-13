package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"cli_app/internal/apiclient"
	"cli_app/internal/config"
)

// enumFlag implements pflag.Value to restrict a flag to a fixed set of choices.
type enumFlag struct {
	value   string
	allowed []string
}

func newEnumFlag(allowed []string) *enumFlag {
	return &enumFlag{allowed: allowed}
}

func (e *enumFlag) String() string { return e.value }

func (e *enumFlag) Set(val string) error {
	for _, a := range e.allowed {
		if val == a {
			e.value = val
			return nil
		}
	}
	return fmt.Errorf("must be one of: %s", strings.Join(e.allowed, ", "))
}

func (e *enumFlag) Type() string { return "string" }

var allowedEmbedModels = []string{
	"text-embedding-ada-002",
	"text-embedding-3-small",
	"text-embedding-3-large",
}

var allowedInferModels = []string{
	"GPT-4o",
	"LLama-3.1-8B-Instruct",
	"DeepSeek-R1",
}

var (
	datapackName       string
	datapackEmbedModel = newEnumFlag(allowedEmbedModels)
	datapackInferModel = newEnumFlag(allowedInferModels)
	datapackKbase      string
	jsonOutput         bool
	forceDelete        bool
	storedCreds        *config.Credentials
)

func newAPIClient() *apiclient.Client {
	client := apiclient.NewFromBaseURL(storedCreds.APIBaseURL)
	client.SetAuthToken(storedCreds.AccessToken)
	return client
}

// requireLogin loads stored credentials and fails if the user has not
// logged in. It also chains the root command's PersistentPreRunE so
// config and logging are initialised.
func requireLogin(cmd *cobra.Command, args []string) error {
	if rootCmd.PersistentPreRunE != nil {
		if err := rootCmd.PersistentPreRunE(cmd, args); err != nil {
			return err
		}
	}

	creds, err := config.LoadCredentials()
	if err != nil {
		slog.Error("failed to load credentials", "error", err)
		return fmt.Errorf("failed to load credentials: %w", err)
	}
	if creds == nil {
		slog.Warn("no credentials found, login required")
		return fmt.Errorf("not logged in — run `meko login` first")
	}

	slog.Info("authenticated session", "email", creds.Email,
		"server", creds.APIBaseURL)
	storedCreds = creds
	return nil
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func numVal(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

// printRawJSON prints indented JSON from a raw message.
func printRawJSON(data json.RawMessage) error {
	return printRawJSONTo(os.Stdout, data)
}

func printRawJSONTo(w io.Writer, data json.RawMessage) error {
	var pretty json.RawMessage
	if err := json.Unmarshal(data, &pretty); err != nil {
		return err
	}
	out, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(w, string(out))
	return nil
}

// spinnerResult holds the outcome of a function executed with a spinner.
type spinnerResult struct {
	data json.RawMessage
	err  error
}

type spinnerDoneMsg spinnerResult

// spinnerModel is a minimal Bubble Tea model that shows a spinner while
// a background function runs.
type spinnerModel struct {
	spinner spinner.Model
	message string
	done    bool
	result  spinnerResult
}

func newSpinnerModel(msg string) spinnerModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4"))
	return spinnerModel{spinner: s, message: msg}
}

func (m spinnerModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m spinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinnerDoneMsg:
		m.done = true
		m.result = spinnerResult(msg)
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.result.err = fmt.Errorf("interrupted")
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m spinnerModel) View() string {
	if m.done {
		return ""
	}
	return fmt.Sprintf("  %s %s\n", m.spinner.View(), m.message)
}

// withSpinner runs fn in the background while showing an animated
// spinner. Returns the raw JSON data and any error from fn.
func withSpinner(message string, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	model := newSpinnerModel(message)
	p := tea.NewProgram(model)

	go func() {
		data, err := fn()
		p.Send(spinnerDoneMsg{data: data, err: err})
	}()

	final, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("spinner error: %w", err)
	}

	result := final.(spinnerModel).result
	return result.data, result.err
}

// createResponse mirrors the API server's CreateResponse for proper
// JSON unmarshalling of nested objects.
type createResponse struct {
	DatapackID string   `json:"datapack_id"`
	AgentID    string   `json:"agent_id"`
	Name       string   `json:"name"`
	MCPServer  mcpInfo  `json:"mcp_server"`
	DBAccess   dbAccess `json:"db_access"`
}

type mcpInfo struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

type dbAccess struct {
	UserName string `json:"user_name"`
	Password string `json:"password"`
	ConnStr  string `json:"conn_str"`
}

func printCreateResult(resp *createResponse) {
	line := "────────────────────────────────────────────────────────"
	fmt.Println()
	fmt.Printf("  ✅ Datapack Created\n")
	fmt.Println()
	fmt.Printf("  %-16s %s\n", "Name", resp.Name)
	fmt.Printf("  %-16s %s\n", "Datapack ID", resp.DatapackID)
	fmt.Printf("  %-16s %s\n", "Agent ID", resp.AgentID)
	fmt.Println()
	fmt.Printf("  %s\n", line)
	fmt.Printf("  MCP Server\n")
	fmt.Printf("  %s\n", line)
	fmt.Printf("  %-16s %s\n", "URL", resp.MCPServer.URL)
	fmt.Printf("  %-16s %s\n", "API Key", resp.MCPServer.APIKey)
	fmt.Println()
	fmt.Printf("  %s\n", line)
	fmt.Printf("  Database Access\n")
	fmt.Printf("  %s\n", line)
	fmt.Printf("  %-16s %s\n", "User", resp.DBAccess.UserName)
	fmt.Printf("  %-16s %s\n", "Password", resp.DBAccess.Password)
	fmt.Println()
	fmt.Printf("  %s\n", resp.DBAccess.ConnStr)
	fmt.Println()
}

var datapackCmd = &cobra.Command{
	Use:               "datapack",
	Short:             "Manage datapacks",
	Long:              `Manage datapacks via the meko API server. Requires a valid login session.`,
	PersistentPreRunE: requireLogin,
}

var datapackCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new datapack",
	Long:  `Create a new datapack by name.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		client := newAPIClient()
		var data json.RawMessage
		var err error

		if jsonOutput {
			data, err = client.CreateDatapack(datapackName)
			if err != nil {
				return fmt.Errorf("failed to create datapack: %w", err)
			}
			return printRawJSON(data)
		}

		data, err = withSpinner(
			fmt.Sprintf("Creating datapack %q...", datapackName),
			func() (json.RawMessage, error) {
				return client.CreateDatapack(datapackName)
			},
		)
		if err != nil {
			return fmt.Errorf("failed to create datapack: %w", err)
		}

		var resp createResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		printCreateResult(&resp)
		return nil
	},
}

// Page sizes for datapack list. --all reads with the largest page the API
// server serves unless --limit says otherwise, so reading a large account
// takes as few requests as possible.
const (
	defaultListLimit = 50
	allListLimit     = 1000
)

var (
	listSearch string
	listLimit  int
	listCursor string
	listAll    bool
)

var datapackListCmd = &cobra.Command{
	Use:   "list",
	Short: "List datapacks, one page at a time",
	Long: `List the datapacks you own or that are shared with you, newest first.

By default this prints one page of --limit rows and the command that prints the
next page. Use --all to print every page.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts := datapackListOptions{
			Search: listSearch,
			Limit:  listLimit,
			Cursor: listCursor,
			All:    listAll,
			JSON:   jsonOutput,
		}
		if listAll && !cmd.Flags().Changed("limit") {
			opts.Limit = allListLimit
		}
		return runDatapackList(newAPIClient(), opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

type datapackListOptions struct {
	Search string
	Cursor string
	Limit  int
	All    bool
	JSON   bool
}

// runDatapackList prints one page, or every page with All. An API server that
// predates paged mode answers with a bare array: that is the complete list, so
// it is printed whole, with --search applied here because that server ignores q.
//
// --json always prints a JSON array on w. fields=full keeps the item fields
// the old bare array had (connection fields stay redacted). The next-page
// command goes to errW so a script can read the array from stdout.
func runDatapackList(c *apiclient.Client, opts datapackListOptions, w, errW io.Writer) error {
	if opts.Limit < 1 {
		return fmt.Errorf("--limit must be at least 1")
	}
	if errW == nil {
		errW = os.Stderr
	}
	params := apiclient.DatapackListParams{
		Limit: opts.Limit, Cursor: opts.Cursor, Search: opts.Search, Fields: "full",
	}

	var rows []map[string]any
	var last *apiclient.DatapackPage
	for {
		page, err := c.ListDatapacksPage(params)
		if err != nil {
			return fmt.Errorf("failed to list datapacks: %w", err)
		}
		last = page
		if page.Complete {
			// A bare array is the whole list, so it replaces any pages read
			// before it (an old replica answering mid-rollout).
			rows = filterByName(page.Items, opts.Search)
			break
		}
		if !opts.All {
			rows = page.Items
			break
		}
		rows = append(rows, page.Items...)
		if !page.HasMore {
			break
		}
		if page.NextCursor == params.Cursor {
			return fmt.Errorf("failed to list datapacks: the server repeated cursor %q", page.NextCursor)
		}
		params.Cursor = page.NextCursor
	}

	if opts.JSON {
		if rows == nil {
			rows = []map[string]any{}
		}
		out, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(out))
	} else if len(rows) == 0 {
		fmt.Fprintln(w, "No datapacks found.")
	} else {
		fmt.Fprintf(w, "%-30s %-36s %-20s\n", "NAME", "ID", "CREATED")
		for _, dp := range rows {
			created := strVal(dp, "created_at")
			if len(created) > 19 {
				created = created[:19]
			}
			fmt.Fprintf(w, "%-30s %-36s %-20s\n",
				strVal(dp, "datapack_name"),
				strVal(dp, "datapack_id"),
				created)
		}
	}
	if !opts.All && !last.Complete && last.HasMore {
		hint := fmt.Sprintf("\nMore datapacks exist. Next page:\n  %s\nOr use --all to list every page.\n",
			nextPageCommand(opts, last.NextCursor))
		if opts.JSON {
			fmt.Fprint(errW, hint)
		} else {
			fmt.Fprint(w, hint)
		}
	}
	return nil
}

// nextPageCommand repeats the flags that shape the listing, because the cursor
// is only valid for the same search.
func nextPageCommand(opts datapackListOptions, cursor string) string {
	parts := []string{"meko datapack list"}
	if opts.Search != "" {
		parts = append(parts, "--search "+shellQuote(opts.Search))
	}
	if opts.Limit != defaultListLimit {
		parts = append(parts, fmt.Sprintf("--limit %d", opts.Limit))
	}
	if opts.JSON {
		parts = append(parts, "--json")
	}
	parts = append(parts, "--cursor "+shellQuote(cursor))
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// filterByName applies --search to an old server's full list the way the API
// server's q does: a case-insensitive substring of datapack_name.
func filterByName(rows []map[string]any, search string) []map[string]any {
	search = strings.TrimSpace(search)
	if search == "" {
		return rows
	}
	needle := strings.ToLower(search)
	var out []map[string]any
	for _, dp := range rows {
		if strings.Contains(strings.ToLower(strVal(dp, "datapack_name")), needle) {
			out = append(out, dp)
		}
	}
	return out
}

var datapackDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		if !forceDelete {
			fmt.Printf("WARNING: This will permanently delete datapack %q and its database.\n", datapackName)
			fmt.Printf("To confirm, type the datapack name: ")
			reader := bufio.NewReader(os.Stdin)
			confirm, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("failed to read confirmation: %w", err)
			}
			if strings.TrimSpace(confirm) != datapackName {
				return fmt.Errorf("name does not match, aborting delete")
			}
		}

		if _, err := newAPIClient().DeleteDatapack(datapackName); err != nil {
			return fmt.Errorf("failed to delete datapack: %w", err)
		}

		fmt.Printf("Datapack %q deleted.\n", datapackName)
		return nil
	},
}

var datapackDescribeCmd = &cobra.Command{
	Use:   "describe",
	Short: "Describe a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		data, err := newAPIClient().DescribeDatapack(datapackName)
		if err != nil {
			return fmt.Errorf("failed to describe datapack: %w", err)
		}

		if jsonOutput {
			return printRawJSON(data)
		}

		var dp map[string]any
		if err := json.Unmarshal(data, &dp); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		fmt.Printf("Name            : %s\n", strVal(dp, "name"))
		fmt.Printf("ID              : %s\n", strVal(dp, "id"))
		fmt.Printf("LLM Model       : %s\n", strVal(dp, "llm_model"))
		fmt.Printf("Embedding Model : %s\n",
			strVal(dp, "embedding_model"))
		fmt.Printf("Created         : %s\n",
			strVal(dp, "created_at"))
		return nil
	},
}

var datapackStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show datapack status",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		data, err := newAPIClient().DatapackStatus(datapackName)
		if err != nil {
			return fmt.Errorf("failed to get status: %w", err)
		}

		if jsonOutput {
			return printRawJSON(data)
		}

		var st map[string]any
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		fmt.Printf("Datapack        : %s\n", strVal(st, "name"))
		fmt.Printf("ID              : %s\n", strVal(st, "id"))
		fmt.Printf("Agent Count     : %.0f\n",
			numVal(st, "agent_count"))
		fmt.Printf("KB Count        : %.0f\n",
			numVal(st, "knowledge_base_count"))
		fmt.Printf("Created         : %s\n",
			strVal(st, "created_at"))
		return nil
	},
}

var datapackConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect to a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		if _, err := newAPIClient().DescribeDatapack(datapackName); err != nil {
			return fmt.Errorf("datapack not reachable: %w", err)
		}

		fmt.Printf("Connected to datapack %q\n", datapackName)
		fmt.Printf("  MCP endpoint: https://localhost:5435/mcp/%s\n",
			datapackName)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(datapackCmd)
	datapackCmd.AddCommand(datapackCreateCmd)
	datapackCmd.AddCommand(datapackListCmd)
	datapackCmd.AddCommand(datapackDeleteCmd)
	// datapackCmd.AddCommand(datapackDescribeCmd)
	datapackCmd.AddCommand(datapackStatusCmd)
	// datapackCmd.AddCommand(datapackConnectCmd)

	datapackCmd.PersistentFlags().BoolVar(
		&jsonOutput, "json", false, "output raw JSON")

	// datapackCreateCmd.Flags().Var(
	// 	datapackEmbedModel, "embed_model", "embedding model")
	// datapackCreateCmd.Flags().Var(
	// 	datapackInferModel, "inference_model", "inference model")
	// datapackCreateCmd.Flags().StringVar(
	// 	&datapackKbase, "seed_knowledge_base", "",
	// 	"knowledge base URL (required)")

	datapackListCmd.Flags().StringVar(
		&listSearch, "search", "", "only datapacks whose name contains this text (case-insensitive)")
	datapackListCmd.Flags().IntVar(
		&listLimit, "limit", defaultListLimit, "datapacks per page (the server caps it at 1000)")
	datapackListCmd.Flags().StringVar(
		&listCursor, "cursor", "", "start after this cursor, from a previous page")
	datapackListCmd.Flags().BoolVar(
		&listAll, "all", false, "list every page")

	datapackDeleteCmd.Flags().BoolVarP(
		&forceDelete, "force", "f", false, "Skip confirmation prompt")

	for _, sub := range []*cobra.Command{
		datapackCreateCmd, datapackDeleteCmd,
		datapackDescribeCmd, datapackStatusCmd,
	} {
		sub.Flags().StringVar(
			&datapackName, "name", "", "datapack name (required)")
	}

	//nolint:errcheck // flag completions are best-effort
	// datapackCreateCmd.RegisterFlagCompletionFunc("embed_model",
	// 	func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	// 		return allowedEmbedModels, cobra.ShellCompDirectiveNoFileComp
	// 	})
	//nolint:errcheck // flag completions are best-effort
	// datapackCreateCmd.RegisterFlagCompletionFunc("inference_model",
	// 	func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	// 		return allowedInferModels, cobra.ShellCompDirectiveNoFileComp
	// 	})
}
