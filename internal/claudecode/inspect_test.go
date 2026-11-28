package claudecode

import (
	"strings"
	"testing"
)

func TestEncodeDirForClaude(t *testing.T) {
	cases := []struct {
		dir  string
		want string
	}{
		{`C:\Users\LYX\AppData\Local\Temp`, `C--Users-LYX-AppData-Local-Temp`},
		{`/home/user/work`, `-home-user-work`},
		{`C:\Users\LYX\Desktop\Workstave`, `C--Users-LYX-Desktop-Workstave`},
	}
	for _, c := range cases {
		if got := encodeDirForClaude(c.dir); got != c.want {
			t.Errorf("encodeDirForClaude(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestInjectionDetected(t *testing.T) {
	cases := []struct {
		desc      string
		cliPrefix string
		local     []string
		wire      []string
		want      bool
	}{
		{
			desc:      "clean: wire merges cliPrefix + local into two blocks",
			cliPrefix: "You are Claude Code.",
			local:     []string{"Do X.", "Do Y."},
			wire:      []string{"You are Claude Code.", "Do X.Do Y."},
			want:      false,
		},
		{
			desc:      "clean: extra whitespace differences are normalised away",
			cliPrefix: "You are Claude Code.",
			local:     []string{"\nDo X.\n", "Do Y."},
			// wire[1] is local segments concatenated verbatim, with no added spaces
			wire: []string{"You are Claude Code.", "\nDo X.\nDo Y."},
			want: false,
		},
		{
			desc:      "injection: gateway appended extra text",
			cliPrefix: "You are Claude Code.",
			local:     []string{"Do X."},
			wire:      []string{"You are Claude Code.", "Do X.", "INJECTED"},
			want:      true,
		},
		{
			desc:      "injection: gateway replaced a segment",
			cliPrefix: "You are Claude Code.",
			local:     []string{"Do X."},
			wire:      []string{"You are Claude Code.", "Do SOMETHING ELSE."},
			want:      true,
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := injectionDetected(c.cliPrefix, c.local, c.wire)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestNewInspectIDIsValidUUID(t *testing.T) {
	id, err := newInspectID()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("newInspectID() = %q, want UUID with 5 hyphen-separated parts", id)
	}
	lengths := []int{8, 4, 4, 4, 12}
	for i, part := range parts {
		if len(part) != lengths[i] {
			t.Errorf("part %d %q: length %d, want %d", i, part, len(part), lengths[i])
		}
	}
	// version nibble must be 4
	if parts[2][0] != '4' {
		t.Errorf("UUID version nibble = %c, want 4", parts[2][0])
	}
}
