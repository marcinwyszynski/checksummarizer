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

	// Get from the environment the ID of the app installation you want to spy on.
	observedAppName, ok := os.LookupEnv("OBSERVED_APP_NAME")
	if !ok {
		log.Panic("OBSERVED_APP_NAME not set")

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
		observedAppName,
	)))
}

func BuildHandler(client *github.Client, secretToken []byte, observedAppName string) http.HandlerFunc {
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
			if err := handleCheckRunEvent(r.Context(), client.Checks, event, observedAppName); err != nil {
				log.Printf("Error handling check run event: %v", err)
				http.Error(w, "Error handling check run event", http.StatusInternalServerError)
			}
		default:
			log.Printf("Received event of ignored type %q", github.WebHookType(r))
		}
	}
}

func handleCheckRunEvent(ctx context.Context, service *github.ChecksService, event *github.CheckRunEvent, observedAppName string) error {
	// App not interesting, ignore.
	if appName := event.GetCheckRun().GetApp().GetName(); appName != observedAppName {
		log.Printf("Received check run event for ignored app %q", appName)
		return nil
	}

	checkStatus := event.GetCheckRun().GetStatus()
	checkName := event.GetCheckRun().GetName()
	checkConclusion := event.GetCheckRun().GetConclusion()

	// The check has not finished yet, nothing to do.
	if checkConclusion == "" {
		log.Printf("No conclusion yet for %s check run %s", checkStatus, checkName)
		return nil
	}

	appID := event.GetCheckRun().GetApp().GetID()
	commitSHA := event.GetCheckRun().GetHeadSHA()
	owner := event.GetRepo().GetOwner().GetLogin()
	repoName := event.GetRepo().GetName()

	log.Printf("Received check run event for %s check run %s with conclusion %s", checkStatus, checkName, checkConclusion)

	// List check runs for the commit.
	result, response, err := service.ListCheckRunsForRef(
		ctx,
		owner,
		repoName,
		commitSHA,
		&github.ListCheckRunsOptions{AppID: &appID},
	)

	if err != nil {
		return fmt.Errorf("error listing check runs for commit %s: %v", commitSHA, err)
	}

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("error listing check runs for commit %s: %d", commitSHA, response.StatusCode)
	}

	type check struct {
		name, detailsURL string
	}

	var success, failure, inProgress []check

	for _, checkRun := range result.CheckRuns {
		check := check{
			name:       checkRun.GetName(),
			detailsURL: checkRun.GetDetailsURL(),
		}

		switch checkRun.GetStatus() {
		case "completed":
			switch checkRun.GetConclusion() {
			case "failure", "cancelled", "stale", "timed_out":
				failure = append(failure, check)
			case "action_required":
				inProgress = append(inProgress, check)
			default:
				success = append(success, check)
			}
		default:
			inProgress = append(inProgress, check)
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

	appendChecks := func(name string, collection []check) {
		summaryParts = append(summaryParts, fmt.Sprintf("## %s:", name))
		for _, check := range collection {
			summaryParts = append(summaryParts, fmt.Sprintf("- [%s](%s);", check.name, check.detailsURL))
		}
		summaryParts = append(summaryParts, "")
	}

	if len(success) > 0 {
		titleParts = append(titleParts, fmt.Sprintf("%d successful", len(success)))
		appendChecks("Successful checks", success)
	}

	if len(failure) > 0 {
		titleParts = append(titleParts, fmt.Sprintf("%d failed", len(failure)))
		appendChecks("Failed checks", failure)
	}

	if len(inProgress) > 0 {
		titleParts = append(titleParts, fmt.Sprintf("%d in progress", len(inProgress)))
		appendChecks("Checks in progress", inProgress)
	}

	title := strings.Join(titleParts, ", ")
	summary := strings.Join(summaryParts, "\n")

	_, response, err = service.CreateCheckRun(
		ctx,
		owner,
		repoName,
		github.CreateCheckRunOptions{
			Name:       fmt.Sprintf("Synthetic status for %s", observedAppName),
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
