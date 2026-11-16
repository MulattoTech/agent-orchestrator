package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type publishReviewRequest struct {
	RunID string `json:"runId"`
}
type publishReviewResponse struct {
	Review            reviewRun `json:"review"`
	ProviderState     string    `json:"providerState"`
	ProviderReviewURL string    `json:"providerReviewUrl"`
	Reused            bool      `json:"reused"`
}

func newReviewPublishCommand(ctx *commandContext) *cobra.Command {
	var session, runID string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "publish [worker-session-id]",
		Short: "Publish a saved review body without starting model inference",
		Long:  "Publish or verify the saved review body on its exact PR head. A COMMENTED GitHub review is not a branch approval. Replays reuse a matching provider receipt; they never start a reviewer or reconstruct missing inline comments.",
		Args:  atMostOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			worker := reviewTargetSession(args, session)
			if worker == "" || strings.TrimSpace(runID) == "" {
				return usageError{errors.New("usage: worker session and --run are required")}
			}
			var result publishReviewResponse
			if err := ctx.postJSON(cmd.Context(), "sessions/"+url.PathEscape(worker)+"/reviews/publish", publishReviewRequest{RunID: strings.TrimSpace(runID)}, &result); err != nil {
				return err
			}
			if result.Review.ID == "" || result.Review.GithubReviewID == "" || result.Review.TargetSHA == "" || result.ProviderState == "" {
				return fmt.Errorf("daemon returned an incomplete publication receipt")
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), result)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "saved AO verdict %s; GitHub review state %s; commit %s; review %s\n", result.Review.Verdict, result.ProviderState, result.Review.TargetSHA, result.Review.GithubReviewID)
			return err
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Worker session id")
	cmd.Flags().StringVar(&runID, "run", "", "Saved complete review run id (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the verified publication receipt as JSON")
	return cmd
}
