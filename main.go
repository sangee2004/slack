package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

type args struct {
	Command     string `json:"command"`
	Query       string `json:"query"`
	WithContext string `json:"withContext"`
}

func main() {
	if os.Getenv("GPTSCRIPT_SLACK_TOKEN") == "" {
		logrus.Error("GPTSCRIPT_SLACK_TOKEN is not set")
		os.Exit(1)
	}

	slackClient := slack.New(os.Getenv("GPTSCRIPT_SLACK_TOKEN"))

	if len(os.Args) != 2 {
		logrus.Errorf("Usage: %s <JSON parameters>", os.Args[0])
		os.Exit(1)
	}

	var a args
	if err := json.Unmarshal([]byte(os.Args[1]), &a); err != nil {
		logrus.Errorf("failed to parse arguments: %v", err)
		os.Exit(1)
	}

	var err error
	switch a.Command {
	case "search_messages":
		if a.Query == "" {
			logrus.Error("query is required for search")
			os.Exit(1)
		}
		err = search(slackClient, a.Query, a.WithContext == "true")
	case "list_channels":
		err = listChannels(slackClient)
	case "list_users":
		err = listUsers(slackClient)
	default:
		logrus.Errorf("unknown command: %s", a.Command)
		os.Exit(1)
	}

	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
}

func listChannels(s *slack.Client) error {
	channels, nextCursor, err := s.GetConversations(&slack.GetConversationsParameters{
		ExcludeArchived: true,
	})
	if err != nil {
		return fmt.Errorf("failed to list channels: %w", err)
	}

	for nextCursor != "" {
		var moreChannels []slack.Channel
		moreChannels, nextCursor, err = s.GetConversations(&slack.GetConversationsParameters{
			ExcludeArchived: true,
			Cursor:          nextCursor,
		})
		if err != nil {
			return fmt.Errorf("failed to list channels: %w", err)
		}
		channels = append(channels, moreChannels...)
	}

	fmt.Println("Channels:")
	for _, c := range channels {
		printChannel(c)
	}
	return nil
}

func printChannel(c slack.Channel) {
	fmt.Printf("- %s (ID: %s)\n", c.Name, c.ID)
	if c.Topic.Value != "" {
		fmt.Printf("  Topic: %s\n", c.Topic.Value)
	}
}

func search(s *slack.Client, q string, withContext bool) error {
	messages, err := s.SearchMessages(q, slack.SearchParameters{
		SortDirection: "desc",
		Count:         100,
	})
	if err != nil {
		return fmt.Errorf("failed to search: %w", err)
	}

	slices.Reverse(messages.Matches)
	for _, m := range messages.Matches {
		if withContext {
			if err := printMessageWithContext(s, m); err != nil {
				return err
			}
			fmt.Println() // Add an empty line between context blocks
		} else {
			if err := printSearchMessage(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func printMessageWithContext(s *slack.Client, m slack.SearchMessage) error {
	userMap, err := userIdToUsernameMap(s)
	if err != nil {
		return err
	}

	beforeHist, err := s.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: m.Channel.ID,
		Inclusive: true,
		Latest:    m.Timestamp,
		Limit:     4,
	})
	if err != nil {
		return fmt.Errorf("failed to get conversation history: %w", err)
	}

	afterHist, err := s.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: m.Channel.ID,
		Inclusive: false,
		Oldest:    m.Timestamp,
		Limit:     3,
	})
	if err != nil {
		return fmt.Errorf("failed to get conversation history: %w", err)
	}

	messages := append(beforeHist.Messages, afterHist.Messages...)
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Timestamp < messages[j].Timestamp
	})

	// Look through the messages and attempt to find one that matches the search result
	var found bool
	for _, msg := range messages {
		if msg.Timestamp == m.Timestamp && msg.User == m.User && msg.Text == m.Text {
			found = true
			break
		}
	}

	if !found {
		if strings.Contains(m.Permalink, "thread_ts") {
			// Turns out that the search result is a reply in a thread, so we need to print from the thread
			return printReplyWithContext(s, m, userMap)
		}
		return nil // Shouldn't happen
	}

	for _, msg := range messages {
		if err := printMessage(msg, m.Channel.Name, userMap); err != nil {
			return err
		}
	}

	return nil
}

func printMessage(m slack.Message, channel string, userMap map[string]string) error {
	unixTimeSeconds, err := strconv.ParseFloat(m.Timestamp, 64)
	if err != nil {
		return fmt.Errorf("failed to parse timestamp: %w", err)
	}

	t := time.UnixMicro(int64(unixTimeSeconds * 1000000))
	fmt.Printf("[%s] %s in #%s: %q\n", t.Format(time.DateTime), userMap[m.User], channel, m.Text)

	return nil
}

func printSearchMessage(m slack.SearchMessage) error {
	unixTimeSeconds, err := strconv.ParseFloat(m.Timestamp, 64)
	if err != nil {
		return fmt.Errorf("failed to parse timestamp: %w", err)
	}

	t := time.UnixMicro(int64(unixTimeSeconds * 1000000))
	fmt.Printf("[%s] %s in #%s: %q\n", t.Format(time.DateTime), m.Username, m.Channel.Name, m.Text)

	return nil
}

func printReplyWithContext(s *slack.Client, m slack.SearchMessage, userMap map[string]string) error {
	// The message is in a thread somewhere, so let's get it.
	permaURL, err := url.Parse(m.Permalink)
	if err != nil {
		return fmt.Errorf("failed to parse permalink: %w", err)
	}

	more := true
	var (
		replies    []slack.Message
		nextCursor string
	)

	for more {
		var currReplies []slack.Message
		currReplies, more, nextCursor, err = s.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: m.Channel.ID,
			Inclusive: true,
			Timestamp: permaURL.Query().Get("thread_ts"),
			Cursor:    nextCursor,
		})
		if err != nil {
			return fmt.Errorf("failed to get conversation replies: %w", err)
		}

		for j, reply := range currReplies {
			if reply.Timestamp == m.Timestamp && reply.User == m.User && reply.Text == m.Text {
				// We found it!
				lower := j + len(replies) - 3
				if lower < 1 {
					lower = 1
				}
				upper := j + len(replies) + 4
				replies = append(replies, currReplies...)
				if upper > len(replies) {
					upper = len(replies)
				}

				if err := printMessage(replies[0], m.Channel.Name, userMap); err != nil {
					return err
				}
				fmt.Println("    Replies:")
				for _, r := range replies[lower:upper] {
					fmt.Print("    ")
					if err := printMessage(r, m.Channel.Name, userMap); err != nil {
						return err
					}
				}
				return nil
			}
		}
		replies = append(replies, currReplies...)
	}
	return nil
}

func listUsers(s *slack.Client) error {
	users, err := getUserList(s)
	if err != nil {
		return err
	}

	fmt.Println("Users:")
	for _, u := range users {
		printUser(u)
	}
	return nil
}

func userIdToUsernameMap(s *slack.Client) (map[string]string, error) {
	users, err := getUserList(s)
	if err != nil {
		return nil, err
	}

	m := make(map[string]string)
	for _, u := range users {
		m[u.ID] = u.Name
	}
	return m, nil
}

func getUserList(s *slack.Client) ([]slack.User, error) {
	users, err := s.GetUsers()
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	return users, nil
}

func printUser(u slack.User) {
	fmt.Printf("- %s (Name: %s) (ID: %s)\n", u.Name, u.RealName, u.ID)
}
