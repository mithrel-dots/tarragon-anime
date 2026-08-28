package anime

import "testing"

func TestParseQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Query
		err   bool
	}{
		{name: "bare search", input: "one piece", want: Query{Command: CommandSearch, Text: "one piece"}},
		{name: "explicit search", input: " search  one piece ", want: Query{Command: CommandSearch, Text: "one piece"}},
		{name: "episodes", input: "episodes 154587", want: Query{Command: CommandEpisodes, MediaID: 154587}},
		{name: "play", input: "play 154587 4", want: Query{Command: CommandPlay, MediaID: 154587, Episode: 4}},
		{name: "bad episodes", input: "episodes nope", err: true},
		{name: "bad play", input: "play 1 0", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseQuery(test.input)
			if (err != nil) != test.err {
				t.Fatalf("ParseQuery() error = %v, want error %v", err, test.err)
			}
			if got != test.want {
				t.Fatalf("ParseQuery() = %#v, want %#v", got, test.want)
			}
		})
	}
}
