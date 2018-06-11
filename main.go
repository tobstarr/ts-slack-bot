package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-github/github"
	shellwords "github.com/mattn/go-shellwords"
	"github.com/nlopes/slack"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
	"golang.org/x/oauth2"
)

func main() {
	l := log.New(os.Stderr, "", 0)
	h, err := newHandler()
	if err != nil {
		log.Fatal(err)
	}
	if err := h.run(l); err != nil {
		log.Fatal(err)
	}
}

type handler struct {
	SlackToken        string
	GithubToken       string
	GithubOrg         string
	GithubRepo        string
	DockerImagePrefix string
	K8sNamespace      string
	K8sDeployment     string
}

func newHandler() (*handler, error) {
	h := &handler{
		SlackToken:        os.Getenv("SLACK_TOKEN"),
		GithubToken:       os.Getenv("GITHUB_TOKEN"),
		GithubOrg:         os.Getenv("GITHUB_ORG"),
		GithubRepo:        os.Getenv("GITHUB_REPO"),
		DockerImagePrefix: os.Getenv("DOCKER_IMAGE_PREFIX"),
		K8sNamespace:      os.Getenv("K8S_NAMESPACE"),
		K8sDeployment:     os.Getenv("K8S_DEPLOYMENT"),
	}
	// TODO: validate all variables are set
	return h, nil
}

func (h *handler) run(l *log.Logger) error {
	cl := slack.New(h.SlackToken)

	rtm := cl.NewRTM()
	go rtm.ManageConnection()

	rsp, err := cl.AuthTest()
	if err != nil {
		return err
	}
	l.Printf(rsp.UserID)

	list, err := cl.GetChannels(true)
	if err != nil {
		return errors.WithStack(err)
	}
	chans := []string{}
	for _, c := range list {
		ok := func() bool {
			for _, m := range c.Members {
				if m == rsp.UserID {
					return true
				}
			}
			return false
		}()
		if ok {
			chans = append(chans, c.ID)
		}
	}

	if len(chans) != 1 {
		return errors.Errorf("must only be in one channel")
	}
	channelID := chans[0]

	l.Printf("channel=%s", channelID)

	for c := range rtm.IncomingEvents {
		l.Printf("%s %T", c.Type, c.Data)
		switch cc := c.Data.(type) {
		case *slack.HelloEvent:
			msg := rtm.NewOutgoingMessage("Hi there", channelID)
			rtm.SendMessage(msg)
			_ = cc
		case *slack.MessageEvent:
			if cc.User == rsp.UserID {
				continue
			}
			if !strings.HasPrefix(cc.Text, "!") {
				continue
			}

			printer := printer(func(s string) {
				msg := rtm.NewOutgoingMessage(s, cc.Channel)
				rtm.SendMessage(msg)
			})
			buf := &bytes.Buffer{}
			app := &cli.App{Name: "GDG Bot"}
			app.Commands = []cli.Command{
				{Name: "pods", Action: adapt(printer, podsCmd), Flags: []cli.Flag{
					cli.StringFlag{Name: "namespace"},
				}},
				{
					Name: "deploy", Action: adapt(printer, h.deployCmd),
				},
			}
			app.ExitErrHandler = func(*cli.Context, error) {}
			app.Writer = buf
			app.ErrWriter = buf
			args, err := shellwords.Parse(strings.TrimPrefix(cc.Text, "!"))
			if err != nil {
				printer("error: " + err.Error())
				continue
			}
			app.Run(append([]string{"foo"}, args...))

			if buf.Len() > 0 {
				printer("```" + buf.String() + "```")
			}
		}
	}
	return nil
}

type printer func(string)

func adapt(p printer, f func(printer, *cli.Context) error) func(*cli.Context) error {
	return func(ctx *cli.Context) error {
		return f(p, ctx)
	}
}

func (h *handler) deployCmd(p printer, ctx *cli.Context) error {
	p("about to deploy")

	cl, err := h.githubClientFromENV()
	if err != nil {
		return errors.WithStack(err)
	}
	list, _, err := cl.Repositories.ListCommits(context.Background(), h.GithubOrg, h.GithubRepo, nil)
	if err != nil {
		return errors.WithStack(err)
	}

	sha := func() string {
		for _, c := range list {
			if c.SHA != nil {
				s := *c.SHA
				return s[0:12]
			}
		}
		return ""
	}()
	if sha == "" {
		return errors.Errorf("no sha found")
	}
	p(fmt.Sprintf("%d commits", len(list)))
	image := h.DockerImagePrefix + ":" + sha
	p("deploying image " + image)
	b, err := exec.Command("kubectl", "-n", h.K8sNamespace, "set", "image", "deployments/"+h.K8sDeployment, "*="+image).CombinedOutput()
	if err != nil {
		p("ERROR: " + string(b))
		return nil
	}
	p(string(b))
	b, err = exec.Command("kubectl", "-n", h.K8sNamespace, "rollout", "status", "deployments/"+h.K8sDeployment).CombinedOutput()
	if err != nil {
		p("ERROR: " + string(b))
		return nil
	}
	p(string(b))
	p("finished deployment")
	return nil
}

func (h *handler) githubClientFromENV() (*github.Client, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: h.GithubToken})
	tc := oauth2.NewClient(oauth2.NoContext, ts)
	return github.NewClient(tc), nil
}

func podsCmd(p printer, ctx *cli.Context) error {
	p("about to list pods")
	args := []string{"get", "pods"}
	if n := ctx.String("namespace"); n != "" {
		args = append(args, "-n", n)
	}
	b, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		log.Printf("err=%q", string(b))
	} else {
		p("```" + string(b) + "```")
	}
	return nil
}
