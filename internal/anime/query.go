package anime

import (
	"fmt"
	"strconv"
	"strings"
)

type Command int

const (
	CommandSearch Command = iota
	CommandEpisodes
	CommandPlay
	CommandLogin
	CommandLogout
	CommandList
)

type Query struct {
	Command    Command
	Text       string
	MediaID    int
	Episode    int
	ListStatus string
}

func ParseQuery(input string) (Query, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return Query{Command: CommandSearch}, nil
	}

	fields := strings.Fields(input)
	switch strings.ToLower(fields[0]) {
	case "search":
		if len(fields) == 1 {
			return Query{}, fmt.Errorf("search requires a title")
		}
		return Query{Command: CommandSearch, Text: strings.Join(fields[1:], " ")}, nil
	case "login":
		return Query{Command: CommandLogin}, nil
	case "logout":
		return Query{Command: CommandLogout}, nil
	case "watching", "planning", "completed":
		if len(fields) != 1 {
			return Query{}, fmt.Errorf("%s does not accept arguments", fields[0])
		}
		return Query{Command: CommandList, ListStatus: listStatus(fields[0])}, nil
	case "list":
		if len(fields) != 2 {
			return Query{}, fmt.Errorf("usage: list <watching|planning|completed>")
		}
		status := listStatus(fields[1])
		if status == "" {
			return Query{}, fmt.Errorf("list status must be watching, planning, or completed")
		}
		return Query{Command: CommandList, ListStatus: status}, nil
	case "episodes":
		if len(fields) != 2 {
			return Query{}, fmt.Errorf("usage: episodes <anilist-id>")
		}
		mediaID, err := positiveInt(fields[1], "AniList ID")
		if err != nil {
			return Query{}, err
		}
		return Query{Command: CommandEpisodes, MediaID: mediaID}, nil
	case "play":
		if len(fields) != 3 {
			return Query{}, fmt.Errorf("usage: play <anilist-id> <episode>")
		}
		mediaID, err := positiveInt(fields[1], "AniList ID")
		if err != nil {
			return Query{}, err
		}
		episode, err := positiveInt(fields[2], "episode")
		if err != nil {
			return Query{}, err
		}
		return Query{Command: CommandPlay, MediaID: mediaID, Episode: episode}, nil
	default:
		return Query{Command: CommandSearch, Text: input}, nil
	}
}

func listStatus(value string) string {
	switch strings.ToLower(value) {
	case "watching":
		return "CURRENT"
	case "planning":
		return "PLANNING"
	case "completed":
		return "COMPLETED"
	default:
		return ""
	}
}

func positiveInt(value, label string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", label)
	}
	return n, nil
}
