package mobile

import (
	"bufio"
	"os/exec"
	"strings"
)

type ConnectorItem struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Command string `json:"command"`
}

var fallbackConnectors = []ConnectorItem{
	{Name: "ai-memory-hub", Type: "stdio", Status: "enabled", Command: "ai-memory-hub-mcp"},
	{Name: "instabox", Type: "stdio", Status: "enabled", Command: "instabox mcp start"},
	{Name: "github", Type: "http", Status: "enabled", Command: "https://api.githubcopilot.com/mcp"},
}

func ListMCPConnectors() []ConnectorItem {
	cmd := exec.Command("agy", "mcp", "list")
	out, err := cmd.Output()
	if err != nil {
		return fallbackConnectors
	}

	var list []ConnectorItem
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			cmdStr := ""
			if len(fields) >= 4 {
				cmdStr = strings.Join(fields[3:], " ")
			}
			list = append(list, ConnectorItem{
				Name:    fields[0],
				Type:    fields[1],
				Status:  fields[2],
				Command: cmdStr,
			})
		}
	}
	if len(list) == 0 {
		return fallbackConnectors
	}
	return list
}

func ToggleMCPConnector(name string, enable bool) error {
	action := "disable"
	if enable {
		action = "enable"
	}
	if _, err := exec.LookPath("agy"); err != nil {
		return nil
	}
	return exec.Command("agy", "mcp", action, name).Run()
}
