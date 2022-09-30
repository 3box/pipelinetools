package notifs

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/webhook"
	"github.com/disgoorg/snowflake/v2"

	"github.com/3box/pipeline-tools/cd/manager"
)

type DiscordColor int

const (
	DiscordColor_None    = iota
	DiscordColor_Info    = 3447003
	DiscordColor_Ok      = 3581519
	DiscordColor_Warning = 16776960
	DiscordColor_Alert   = 16711712
)

const DiscordPacing = 2 * time.Second

var _ manager.Notifs = &JobNotifs{}

type JobNotifs struct {
	db                 manager.Database
	cache              manager.Cache
	deploymentsWebhook webhook.Client
	communityWebhook   webhook.Client
	testWebhook        webhook.Client
	env                manager.EnvType
}

func NewJobNotifs(db manager.Database, cache manager.Cache) (manager.Notifs, error) {
	if d, err := parseDiscordWebhookUrl("DISCORD_DEPLOYMENTS_WEBHOOK"); err != nil {
		return nil, err
	} else if c, err := parseDiscordWebhookUrl("DISCORD_COMMUNITY_NODES_WEBHOOK"); err != nil {
		return nil, err
	} else if t, err := parseDiscordWebhookUrl("DISCORD_TEST_WEBHOOK"); err != nil {
		return nil, err
	} else {
		return &JobNotifs{db, cache, d, c, t, manager.EnvType(os.Getenv("ENV"))}, nil
	}
}

func parseDiscordWebhookUrl(urlEnv string) (webhook.Client, error) {
	webhookUrl := os.Getenv(urlEnv)
	if len(webhookUrl) > 0 {
		if parsedUrl, err := url.Parse(webhookUrl); err != nil {
			return nil, err
		} else {
			urlParts := strings.Split(parsedUrl.Path, "/")
			if id, err := snowflake.Parse(urlParts[len(urlParts)-2]); err != nil {
				return nil, err
			} else {
				return webhook.New(id, urlParts[len(urlParts)-1]), nil
			}
		}
	}
	return nil, nil
}

func (n JobNotifs) NotifyJob(jobs ...manager.JobState) {
	for _, jobState := range jobs {
		for _, channel := range n.getNotifChannels(jobState) {
			if channel != nil {
				n.sendNotif(
					n.getNotifTitle(jobState),
					n.getNotifFields(jobState),
					n.getNotifColor(jobState),
					channel,
				)
			}
		}
	}
}

func (n JobNotifs) sendNotif(title string, fields []discord.EmbedField, color DiscordColor, channel webhook.Client) {
	messageEmbed := discord.Embed{
		Title:  title,
		Type:   discord.EmbedTypeRich,
		Fields: fields,
		Color:  int(color),
	}
	if _, err := channel.CreateMessage(discord.NewWebhookMessageCreateBuilder().
		SetEmbeds(messageEmbed).
		SetUsername(manager.ServiceName).
		Build(),
		rest.WithDelay(DiscordPacing),
	); err != nil {
		log.Printf("notifyJob: error sending discord notification: %v, %s, %v, %d", err, title, fields, color)
	}
}

func (n JobNotifs) getNotifChannels(jobState manager.JobState) []webhook.Client {
	webhooks := make([]webhook.Client, 0, 1)
	if jobState.Type == manager.JobType_Deploy {
		webhooks = append(webhooks, n.deploymentsWebhook)
		// Don't send Dev/QA notifications to the community channel.
		if (n.env != manager.EnvType_Dev) && (n.env != manager.EnvType_Qa) {
			webhooks = append(webhooks, n.communityWebhook)
		}
	}
	// Send all notifications to the test webhook.
	webhooks = append(webhooks, n.testWebhook)
	return webhooks
}

func (n JobNotifs) getNotifTitle(jobState manager.JobState) string {
	var jobTitlePfx string
	if jobState.Type == manager.JobType_Deploy {
		component := jobState.Params[manager.JobParam_Component].(string)
		jobTitlePfx = fmt.Sprintf("3Box Labs `%s` %s ", manager.EnvName(n.env), strings.ToUpper(component))
	}
	jobName := manager.JobName(jobState.Type)
	if manual, found := jobState.Params[manager.JobParam_Manual].(bool); found && manual {
		jobName = fmt.Sprintf("%s %s", manager.JobParam_Manual, jobName)
	}
	return fmt.Sprintf("%s%s %s", jobTitlePfx, jobName, strings.ToUpper(string(jobState.Stage)))
}

func (n JobNotifs) getNotifFields(jobState manager.JobState) []discord.EmbedField {
	fields := []discord.EmbedField{
		{
			Name:  manager.NotifField_JobId,
			Value: jobState.Id,
		},
	}
	// Return deploy hashes for all jobs, if we were able to retrieve them successfully.
	if commitHashes := n.getDeployHashes(jobState); len(commitHashes) > 0 {
		fields = append(fields, discord.EmbedField{
			Name:  manager.NotifField_CommitHashes,
			Value: commitHashes,
		})
	}
	fields = append(fields, discord.EmbedField{
		Name:  manager.NotifField_Time,
		Value: time.Now().Format(time.RFC1123), // "Mon, 02 Jan 2006 15:04:05 MST"
	})
	// Add the list of jobs in progress
	if activeJobs := n.getActiveJobs(jobState); len(activeJobs) > 0 {
		fields = append(fields, activeJobs...)
	}
	return fields
}

func (n JobNotifs) getNotifColor(jobState manager.JobState) DiscordColor {
	switch jobState.Stage {
	case manager.JobStage_Queued:
		return DiscordColor_Info
	case manager.JobStage_Skipped:
		return DiscordColor_Warning
	case manager.JobStage_Started:
		return DiscordColor_None
	case manager.JobStage_Waiting:
		return DiscordColor_Info
	case manager.JobStage_Delayed:
		return DiscordColor_Warning
	case manager.JobStage_Failed:
		return DiscordColor_Alert
	case manager.JobStage_Canceled:
		return DiscordColor_Warning
	case manager.JobStage_Completed:
		return DiscordColor_Ok
	default:
		log.Printf("sendNotif: unknown job stage: %s", manager.PrintJob(jobState))
		return DiscordColor_Alert
	}
}

func (n JobNotifs) getDeployHashes(jobState manager.JobState) string {
	if commitHashes, err := n.db.GetDeployHashes(); err != nil {
		return ""
	} else {
		if jobState.Type == manager.JobType_Deploy {
			sha := jobState.Params[manager.JobParam_Sha].(string)
			// If the specified hash is valid, overwrite the previous hash from the database.
			if isValidSha, _ := regexp.MatchString(manager.CommitHashRegex, sha); isValidSha {
				commitHashes[manager.DeployComponent(jobState.Params[manager.JobParam_Component].(string))] = sha
			}
		}
		// Prepare component messages with GitHub commit hashes and hyperlinks
		ceramicMsg := n.getComponentMsg(manager.DeployComponent_Ceramic, commitHashes[manager.DeployComponent_Ceramic])
		casMsg := n.getComponentMsg(manager.DeployComponent_Cas, commitHashes[manager.DeployComponent_Cas])
		ipfsMsg := n.getComponentMsg(manager.DeployComponent_Ipfs, commitHashes[manager.DeployComponent_Ipfs])
		return fmt.Sprintf("%s\n%s\n%s", ceramicMsg, casMsg, ipfsMsg)
	}
}

func (n JobNotifs) getComponentMsg(component manager.DeployComponent, sha string) string {
	repo := manager.ComponentRepo(component)
	return fmt.Sprintf("[%s (%s)](https://github.com/%s/%s/commit/%s)", repo, sha[:12], manager.GitHubOrg, repo, sha)
}

func (n JobNotifs) getActiveJobs(jobState manager.JobState) []discord.EmbedField {
	fields := make([]discord.EmbedField, 0, 0)
	if field, found := n.getActiveJobsByType(jobState, manager.JobType_Deploy); found {
		fields = append(fields, field)
	}
	if field, found := n.getActiveJobsByType(jobState, manager.JobType_Anchor); found {
		fields = append(fields, field)
	}
	if field, found := n.getActiveJobsByType(jobState, manager.JobType_TestE2E); found {
		fields = append(fields, field)
	}
	if field, found := n.getActiveJobsByType(jobState, manager.JobType_TestSmoke); found {
		fields = append(fields, field)
	}
	return fields
}

func (n JobNotifs) getActiveJobsByType(jobState manager.JobState, jobType manager.JobType) (discord.EmbedField, bool) {
	activeJobs := n.cache.JobsByMatcher(func(js manager.JobState) bool {
		return manager.IsActiveJob(js) && (js.Type == jobType)
	})
	message := ""
	for _, activeJob := range activeJobs {
		// Exclude job for which this notification is being generated
		if activeJob.Id != jobState.Id {
			message += fmt.Sprintf("%s (%s)\n", activeJob.Id, activeJob.Stage)
		}
	}
	return discord.EmbedField{
		Name:  manager.NotifField(jobType) + ":",
		Value: message,
	}, len(message) > 0
}
