package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

var (
	kbDatapackName string
	kbURL          string
)

var addKnowledgeBaseCmd = &cobra.Command{
	Use:   "add_knowledge_base",
	Short: "Add a knowledge base to a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if kbDatapackName == "" {
			return fmt.Errorf("--datapack is required")
		}
		if kbURL == "" {
			return fmt.Errorf("--url is required")
		}

		data, err := newAPIClient().AddKnowledgeBase(
			kbDatapackName, kbURL,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to add knowledge base: %w", err)
		}

		var result map[string]string
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		fmt.Printf("Knowledge base added to datapack %q\n",
			kbDatapackName)
		fmt.Printf("  KB Id : %s\n", result["knowledge_base_id"])
		fmt.Printf("  URL   : %s\n", result["url"])
		return nil
	},
}

var deleteKnowledgeBaseCmd = &cobra.Command{
	Use:   "delete_knowledge_base",
	Short: "Delete a knowledge base from a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if kbDatapackName == "" {
			return fmt.Errorf("--datapack is required")
		}
		if kbURL == "" {
			return fmt.Errorf("--url is required")
		}

		if _, err := newAPIClient().DeleteKnowledgeBase(
			kbDatapackName, kbURL,
		); err != nil {
			return fmt.Errorf(
				"failed to delete knowledge base: %w", err)
		}

		fmt.Printf("Knowledge base %q deleted from datapack %q.\n",
			kbURL, kbDatapackName)
		return nil
	},
}

var listKnowledgeBaseCmd = &cobra.Command{
	Use:   "list_knowledge_bases",
	Short: "List all knowledge bases for a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if kbDatapackName == "" {
			return fmt.Errorf("--datapack is required")
		}

		data, err := newAPIClient().ListKnowledgeBases(kbDatapackName)
		if err != nil {
			return fmt.Errorf(
				"failed to list knowledge bases: %w", err)
		}

		var kbs []map[string]any
		if err := json.Unmarshal(data, &kbs); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		if len(kbs) == 0 {
			fmt.Println("No knowledge bases found.")
			return nil
		}

		fmt.Printf("%-50s %-36s %-20s\n", "URL", "ID", "CREATED")
		for _, kb := range kbs {
			created := strVal(kb, "created_at")
			if len(created) > 19 {
				created = created[:19]
			}
			fmt.Printf("%-50s %-36s %-20s\n",
				strVal(kb, "knowledge_base_url"),
				strVal(kb, "id"),
				created)
		}
		return nil
	},
}

func init() {
	// todo: add back in after knowledge base endpoints are implemented
	// datapackCmd.AddCommand(addKnowledgeBaseCmd)
	// datapackCmd.AddCommand(deleteKnowledgeBaseCmd)
	// datapackCmd.AddCommand(listKnowledgeBaseCmd)

	for _, sub := range []*cobra.Command{
		addKnowledgeBaseCmd,
		deleteKnowledgeBaseCmd,
		listKnowledgeBaseCmd,
	} {
		sub.Flags().StringVar(
			&kbDatapackName, "datapack", "",
			"datapack name (required)")
	}

	addKnowledgeBaseCmd.Flags().StringVar(
		&kbURL, "url", "", "knowledge base URL (required)")
	deleteKnowledgeBaseCmd.Flags().StringVar(
		&kbURL, "url", "", "knowledge base URL (required)")
}
