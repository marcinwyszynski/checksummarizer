package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v57/github"
)

func main() {
	// Get the app ID from environment, turn it into an int64.
	appID, err := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)
	if err != nil {
		log.Panicf("Error parsing GITHUB_APP_ID: %v", err)
	}

	// Get the installation ID from environment, turn it into an integer.
	installationID, err := strconv.Atoi(os.Getenv("GITHUB_INSTALLATION_ID"))
	if err != nil {
		log.Panicf("Error parsing GITHUB_INSTALLATION_ID: %v", err)
	}

	// Get the secret token from the environment.
	secretToken, ok := os.LookupEnv("GITHUB_APP_SECRET_TOKEN")
	if !ok {
		log.Panic("GITHUB_APP_PRIVATE_KEY not set")
	}

	// Get from the environment the ID of the app you want to spy on.
	observedAppID, err := strconv.ParseInt(os.Getenv("OBSERVED_APP_ID"), 10, 64)
	if err != nil {
		log.Panicf("Error parsing OBSERVED_APP_ID: %v", err)

	}

	transport, err := ghinstallation.NewKeyFromFile(
		http.DefaultTransport,
		appID,
		int64(installationID),
		os.Getenv("PRIVATE_KEY_PATH"),
	)

	if err != nil {
		log.Panicf("Error creating GitHub App transport: %v", err)
	}

	http.ListenAndServe(":8080", http.HandlerFunc(BuildHandler(
		github.NewClient(&http.Client{Transport: transport}),
		[]byte(secretToken),
		observedAppID,
	)))
}

func BuildHandler(client *github.Client, secretToken []byte, observedAppID int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, err := github.ValidatePayload(r, secretToken)
		if err != nil {
			log.Printf("Error validating payload: %v", err)
			http.Error(w, "Error validating payload", http.StatusBadRequest)
			return
		}

		event, err := github.ParseWebHook(github.WebHookType(r), payload)
		if err != nil {
			log.Printf("Error parsing webhook: %v", err)
			http.Error(w, "Error parsing webhook", http.StatusBadRequest)
			return
		}

		switch event := event.(type) {
		case *github.CheckRunEvent:
			if err := handleCheckRunEvent(r.Context(), client.Checks, event, observedAppID); err != nil {
				log.Printf("Error handling check run event: %v", err)
				http.Error(w, "Error handling check run event", http.StatusInternalServerError)
			}
		default:
			log.Printf("Received event of type %s", github.WebHookType(r))
		}
	}
}

func handleCheckRunEvent(ctx context.Context, service *github.ChecksService, event *github.CheckRunEvent, observedAppID int64) error {
	// App not interesting, ignore.
	if senderID := event.GetCheckRun().GetApp().GetID(); senderID != observedAppID {
		log.Printf("Received check run event for ignored sender %d", senderID)
		return nil
	}

	// The check has not finished yet, nothing to do.
	if event.CheckRun.GetConclusion() == "" {
		log.Printf("No conclusion for check run %d", event.GetCheckRun().GetID())
		return nil
	}

	commitSHA := event.GetCheckRun().GetHeadSHA()
	owner := event.GetRepo().GetOwner().GetLogin()
	repoName := event.GetRepo().GetName()

	// List check runs for the commit.
	result, response, err := service.ListCheckRunsForRef(
		ctx,
		owner,
		repoName,
		commitSHA,
		&github.ListCheckRunsOptions{AppID: &observedAppID},
	)

	if err != nil {
		return fmt.Errorf("error listing check runs for commit %s: %v", commitSHA, err)
	}

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("error listing check runs for commit %s: %d", commitSHA, response.StatusCode)
	}

	var success, failure, inProgress []string

	for _, checkRun := range result.CheckRuns {
		switch checkRun.GetStatus() {
		case "completed":
			switch checkRun.GetConclusion() {
			case "failure", "cancelled", "stale", "timed_out":
				failure = append(failure, checkRun.GetName())
			case "action_required":
				inProgress = append(inProgress, checkRun.GetName())
			default:
				success = append(success, checkRun.GetName())
			}
		default:
			inProgress = append(inProgress, checkRun.GetName())
		}
	}

	var status, conclusionStr string
	if len(failure) > 0 {
		status = "completed"
		conclusionStr = "failure"
	} else if len(inProgress) > 0 {
		status = "in_progress"
	} else {
		status = "completed"
		conclusionStr = "success"
	}

	var conclusion *string
	if conclusionStr != "" {
		conclusion = &conclusionStr
	}

	var titleParts []string
	var summaryParts []string
	if len(success) > 0 {
		titleParts = append(titleParts, fmt.Sprintf("%d successful", len(success)))
		summaryParts = append(summaryParts, "## Successful checks:")
		for _, check := range success {
			summaryParts = append(summaryParts, fmt.Sprintf("- %s", check))
		}
		summaryParts = append(summaryParts, "")
	}

	if len(failure) > 0 {
		titleParts = append(titleParts, fmt.Sprintf("%d failed", len(failure)))
		summaryParts = append(summaryParts, "## Failed checks:")
		for _, check := range failure {
			summaryParts = append(summaryParts, fmt.Sprintf("- %s", check))
		}
		summaryParts = append(summaryParts, "")
	}

	if len(inProgress) > 0 {
		titleParts = append(titleParts, fmt.Sprintf("%d in progress", len(inProgress)))
		summaryParts = append(summaryParts, "## Checks in progress:")
		for _, check := range inProgress {
			summaryParts = append(summaryParts, fmt.Sprintf("- %s", check))
		}
	}

	title := strings.Join(titleParts, ", ")
	summary := strings.Join(summaryParts, "\n")

	_, response, err = service.CreateCheckRun(
		ctx,
		owner,
		repoName,
		github.CreateCheckRunOptions{
			Name:       "Synthetic status",
			HeadSHA:    commitSHA,
			Status:     &status,
			Conclusion: conclusion,
			Output: &github.CheckRunOutput{
				Title:   &title,
				Summary: &summary,
			},
		},
	)

	if err != nil {
		return fmt.Errorf("error creating check run for commit %s: %v", commitSHA, err)
	}

	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("error creating check run for commit %s: %d", commitSHA, response.StatusCode)
	}

	return nil
}
