package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var (
	agentDatapackName string
	agentName         string
	agentMetadata     string
)

var datapackAgentCreateCmd = &cobra.Command{
	Use:   "create_agent",
	Short: "Create an agent in a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if agentDatapackName == "" {
			return fmt.Errorf("--datapack is required")
		}
		if agentName == "" {
			return fmt.Errorf("--agent_name is required")
		}

		data, err := newAPIClient().CreateAgent(
			agentDatapackName, agentName, agentMetadata,
		)
		if err != nil {
			return fmt.Errorf("failed to create agent: %w", err)
		}

		var result map[string]string
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		fmt.Printf("Agent created in datapack %q\n",
			agentDatapackName)
		fmt.Printf("  Agent Name : %s\n", result["agent_name"])
		fmt.Printf("  Agent Id   : %s\n", result["agent_id"])
		return nil
	},
}

var datapackAgentDeleteCmd = &cobra.Command{
	Use:   "delete_agent",
	Short: "Delete an agent from a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if agentDatapackName == "" {
			return fmt.Errorf("--datapack is required")
		}
		if agentName == "" {
			return fmt.Errorf("--agent_name is required")
		}

		if _, err := newAPIClient().DeleteAgent(
			agentDatapackName, agentName,
		); err != nil {
			return fmt.Errorf("failed to delete agent: %w", err)
		}

		fmt.Printf("Agent %q deleted from datapack %q.\n",
			agentName, agentDatapackName)
		return nil
	},
}

var datapackAgentListCmd = &cobra.Command{
	Use:   "list_agents",
	Short: "List all agents in a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if agentDatapackName == "" {
			return fmt.Errorf("--datapack is required")
		}

		data, err := newAPIClient().ListAgents(agentDatapackName)
		if err != nil {
			return fmt.Errorf("failed to list agents: %w", err)
		}

		var agents []map[string]any
		if err := json.Unmarshal(data, &agents); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		if len(agents) == 0 {
			fmt.Println("No agents found.")
			return nil
		}

		fmt.Printf("%-25s %-36s %-20s\n", "NAME", "ID", "CREATED")
		for _, a := range agents {
			created := strVal(a, "created_at")
			if len(created) > 19 {
				created = created[:19]
			}
			fmt.Printf("%-25s %-36s %-20s\n",
				strVal(a, "agent_name"),
				strVal(a, "id"),
				created)
		}
		return nil
	},
}

func init() {
	datapackCmd.AddCommand(datapackAgentCreateCmd)
	datapackCmd.AddCommand(datapackAgentDeleteCmd)
	datapackCmd.AddCommand(datapackAgentListCmd)

	for _, sub := range []*cobra.Command{
		datapackAgentCreateCmd,
		datapackAgentDeleteCmd,
		datapackAgentListCmd,
	} {
		sub.Flags().StringVar(
			&agentDatapackName, "datapack", "",
			"datapack name (required)")
	}

	datapackAgentCreateCmd.Flags().StringVar(
		&agentName, "agent_name", "", "agent name (required)")
	datapackAgentCreateCmd.Flags().StringVar(
		&agentMetadata, "metadata", "", "agent metadata JSON")

	datapackAgentDeleteCmd.Flags().StringVar(
		&agentName, "agent_name", "", "agent name (required)")
}
