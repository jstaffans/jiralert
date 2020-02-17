package notify

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/free/jiralert/pkg/config"
	"github.com/free/jiralert/pkg/template"

	"github.com/andygrunwald/go-jira"
	"github.com/free/jiralert/pkg/alertmanager"
	"github.com/trivago/tgo/tcontainer"
)

// Receiver wraps a JIRA client corresponding to a specific Alertmanager receiver, with its configuration and templates.
type Receiver struct {
	conf   *config.ReceiverConfig
	tmpl   *template.Template
	client *jira.Client
}

// NewReceiver creates a Receiver using the provided configuration and template.
func NewReceiver(c *config.ReceiverConfig, t *template.Template) (*Receiver, error) {
	tp := jira.BasicAuthTransport{
		Username: c.User,
		Password: string(c.Password),
	}
	client, err := jira.NewClient(tp.Client(), c.APIURL)
	if err != nil {
		return nil, err
	}

	return &Receiver{conf: c, tmpl: t, client: client}, nil
}

// Notify implements the Notifier interface.
func (r *Receiver) Notify(data *alertmanager.Data, logger log.Logger) (bool, error) {
	project := r.tmpl.Execute(r.conf.Project, data, logger)
	if err := r.tmpl.Err(); err != nil {
		return false, err
	}
	// Looks like an ALERT metric name, with spaces removed.
	groupID := toGroupID(data.GroupLabels)
	issue, retry, err := r.search(project, groupID, logger)
	if err != nil {
		return retry, err
	}

	issueLabel, err := toIssueLabel(r.conf.LabelKey, data.GroupLabels)
	if err != nil {
		level.Warn(logger).Log("msg", err)
	}

	if issue != nil {
		// The set of JIRA status categories is fixed, this is a safe check to make.
		if issue.Fields.Status.StatusCategory.Key != "done" {
			// Issue is in a "to do" or "in progress" state, all done here.
			level.Debug(logger).Log("msg", "issue is unresolved, nothing to do", "key", issue.Key, "label", groupID)
			return false, nil
		}
		if r.conf.WontFixResolution != "" && issue.Fields.Resolution != nil &&
			issue.Fields.Resolution.Name == r.conf.WontFixResolution {
			// Issue is resolved as "Won't Fix" or equivalent, log a message just in case.
			level.Info(logger).Log("msg", "issue was resolved as won't fix, not reopening", "key", issue.Key, "label", groupID, "resolution", issue.Fields.Resolution.Name)
			return false, nil
		}

		resolutionTime := time.Time(issue.Fields.Resolutiondate)
		if resolutionTime.Add(time.Duration(*r.conf.ReopenDuration)).After(time.Now()) {
			level.Info(logger).Log("msg", "issue was recently resolved, reopening", "key", issue.Key, "label", groupID, "resolution_time", resolutionTime.Format(time.RFC3339), "reopen_duration", *r.conf.ReopenDuration)
			return r.reopen(issue.Key, logger)
		}
	}

	level.Info(logger).Log("msg", "no recent matching issue found, creating new issue", "label", groupID)
	customFields := tcontainer.NewMarshalMap()
	customFields[r.conf.GroupFieldID] = []string{
		groupID,
	}

	issue = &jira.Issue{
		Fields: &jira.IssueFields{
			Project:     jira.Project{Key: project},
			Type:        jira.IssueType{Name: r.tmpl.Execute(r.conf.IssueType, data, logger)},
			Description: r.tmpl.Execute(r.conf.Description, data, logger),
			Summary:     r.tmpl.Execute(r.conf.Summary, data, logger),
			Labels: []string{
				issueLabel,
			},
			Unknowns: customFields,
		},
	}
	if r.conf.Priority != "" {
		issue.Fields.Priority = &jira.Priority{Name: r.tmpl.Execute(r.conf.Priority, data, logger)}
	}

	// Add Components
	if len(r.conf.Components) > 0 {
		issue.Fields.Components = make([]*jira.Component, 0, len(r.conf.Components))
		for _, component := range r.conf.Components {
			issue.Fields.Components = append(issue.Fields.Components, &jira.Component{Name: r.tmpl.Execute(component, data, logger)})
		}
	}

	// Add Labels
	if r.conf.AddGroupLabels {
		for k, v := range data.GroupLabels {
			issue.Fields.Labels = append(issue.Fields.Labels, fmt.Sprintf("%s=%q", k, v))
		}
	}

	if err := r.tmpl.Err(); err != nil {
		return false, err
	}
	retry, err = r.create(issue, logger)
	if err == nil {
		level.Info(logger).Log("msg", "issue created", "key", issue.Key, "id", issue.ID)
	}
	return retry, err
}

// toGroupID returns the group labels in the form of an ALERT metric name, with all spaces removed.
func toGroupID(groupLabels alertmanager.KV) string {
	buf := bytes.NewBufferString("ALERT{")
	for _, p := range groupLabels.SortedPairs() {
		buf.WriteString(p.Name)
		buf.WriteString(fmt.Sprintf("=%q,", p.Value))
	}
	buf.Truncate(buf.Len() - 1)
	buf.WriteString("}")
	return strings.Replace(buf.String(), " ", "", -1)
}

// toIssueLabel extracts the one group label field that we want to use as the Jira label.
func toIssueLabel(labelKey string, groupLabels alertmanager.KV) (string, error) {
	for _, p := range groupLabels.SortedPairs() {
		if p.Name == labelKey {
			return p.Value, nil
		}
	}
	return "", errors.New("label key not found")
}

func (r *Receiver) search(project, groupID string, logger log.Logger) (*jira.Issue, bool, error) {
	query := fmt.Sprintf("project=\"%s\" and %q=%q order by resolutiondate desc", project, r.conf.GroupFieldName, groupID)
	options := &jira.SearchOptions{
		Fields:     []string{"summary", "status", "resolution", "resolutiondate"},
		MaxResults: 2,
	}
	level.Debug(logger).Log("msg", "search", "query", query, "options", options)
	issues, resp, err := r.client.Issue.Search(query, options)
	if err != nil {
		retry, err := handleJiraError("Issue.Search", resp, err, logger)
		return nil, retry, err
	}
	if len(issues) > 0 {
		if len(issues) > 1 {
			// Swallow it, but log a message.
			level.Debug(logger).Log("msg", "  more than one issue matched, picking most recently resolved", "query", query, "issues", issues)
		}

		level.Debug(logger).Log("msg", "  found", "issue", issues[0], "query", query)
		return &issues[0], false, nil
	}
	level.Debug(logger).Log("msg", "  no results", "query", query)
	return nil, false, nil
}

func (r *Receiver) reopen(issueKey string, logger log.Logger) (bool, error) {
	transitions, resp, err := r.client.Issue.GetTransitions(issueKey)
	if err != nil {
		return handleJiraError("Issue.GetTransitions", resp, err, logger)
	}
	for _, t := range transitions {
		if t.Name == r.conf.ReopenState {
			level.Debug(logger).Log("msg", "reopen", "key", issueKey, "transitionID", t.ID)
			resp, err = r.client.Issue.DoTransition(issueKey, t.ID)
			if err != nil {
				return handleJiraError("Issue.DoTransition", resp, err, logger)
			}

			level.Debug(logger).Log("msg", "  done")
			return false, nil
		}
	}
	return false, fmt.Errorf("JIRA state %q does not exist or no transition possible for %s", r.conf.ReopenState, issueKey)
}

func (r *Receiver) create(issue *jira.Issue, logger log.Logger) (bool, error) {
	level.Debug(logger).Log("msg", "create", "issue", *issue)
	newIssue, resp, err := r.client.Issue.Create(issue)
	if err != nil {
		return handleJiraError("Issue.Create", resp, err, logger)
	}
	*issue = *newIssue

	level.Debug(logger).Log("msg", "  done", "key", issue.Key, "id", issue.ID)
	return false, nil
}

func handleJiraError(api string, resp *jira.Response, err error, logger log.Logger) (bool, error) {
	if resp == nil || resp.Request == nil {
		level.Debug(logger).Log("msg", "handleJiraError", "api", api, "err", err)
	} else {
		level.Debug(logger).Log("msg", "handleJiraError", "api", api, "err", err, "url", resp.Request.URL)
	}

	if resp != nil && resp.StatusCode/100 != 2 {
		retry := resp.StatusCode == 500 || resp.StatusCode == 503
		body, _ := ioutil.ReadAll(resp.Body)
		// go-jira error message is not particularly helpful, replace it
		return retry, fmt.Errorf("JIRA request %s returned status %s, body %q", resp.Request.URL, resp.Status, string(body))
	}
	return false, fmt.Errorf("JIRA request %s failed: %s", api, err)
}
