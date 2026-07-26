package main

import "strings"

func walk(node *Node, parent *Node, out *[]nodeWithParent) {
	if node == nil {
		return
	}
	node.Parent = parent
	*out = append(*out, nodeWithParent{node: node, parent: parent})
	for _, child := range node.Nodes {
		walk(child, node, out)
	}
	for _, child := range node.FloatingNodes {
		walk(child, node, out)
	}
}

func isWindow(node *Node) bool {
	if node == nil || node.Type != "con" || node.Name == "" {
		return false
	}
	return node.AppID != nil || node.Window != nil || node.WindowProperties.Class != ""
}

func identifiers(node *Node) []string {
	values := []string{}
	if node.AppID != nil {
		values = append(values, strings.ToLower(*node.AppID))
	}
	if node.WindowProperties.Class != "" {
		values = append(values, strings.ToLower(node.WindowProperties.Class))
	}
	if node.WindowProperties.Instance != "" {
		values = append(values, strings.ToLower(node.WindowProperties.Instance))
	}
	if node.Name != "" {
		values = append(values, strings.ToLower(node.Name))
	}
	return values
}

func iconFor(node *Node) string {
	ids := identifiers(node)
	for _, rule := range iconRules {
		for _, value := range ids {
			if strings.Contains(value, rule.needle) {
				return rule.icon
			}
		}
	}
	return "◆"
}

func appLabel(node *Node) string {
	label := "app"
	if node.AppID != nil && *node.AppID != "" {
		label = *node.AppID
	} else if node.WindowProperties.Class != "" {
		label = node.WindowProperties.Class
	} else if node.WindowProperties.Instance != "" {
		label = node.WindowProperties.Instance
	}
	parts := strings.Split(label, ".")
	label = strings.TrimSpace(parts[len(parts)-1])
	if label == "" {
		return "app"
	}
	runes := []rune(label)
	if len(runes) > 24 {
		runes = runes[:24]
	}
	return string(runes)
}

func textColumns(value string) int {
	return terminalColumns(value)
}

func truncateColumns(value string, maxColumns int) string {
	if maxColumns <= 0 {
		return ""
	}
	if textColumns(value) <= maxColumns {
		return value
	}
	if maxColumns <= 1 {
		return "…"
	}
	used := 0
	out := []rune{}
	for _, r := range value {
		width := terminalColumns(string(r))
		if used+width > maxColumns-1 {
			break
		}
		out = append(out, r)
		used += width
	}
	return string(out) + "…"
}

func tabWidthPX(node *Node, parent *Node) int {
	parentWidth := 0
	if parent != nil {
		parentWidth = parent.Rect.Width
	}
	nodeWidth := node.Rect.Width
	if parent != nil && (parent.Layout == "tabbed" || parent.Layout == "stacked") {
		siblings := 0
		for _, child := range parent.Nodes {
			if isWindow(child) {
				siblings++
			}
		}
		if siblings > 0 && parentWidth > 0 {
			return max(1, parentWidth/siblings)
		}
	}
	return max(1, max(nodeWidth, max(parentWidth, 240)))
}
