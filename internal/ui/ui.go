package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	grid "github.com/achannarasappa/term-grid"
	"github.com/hwyll/ticker/internal/asset"
	c "github.com/hwyll/ticker/internal/common"
	mon "github.com/hwyll/ticker/internal/monitor"
	"github.com/hwyll/ticker/internal/ui/component/summary"
	"github.com/hwyll/ticker/internal/ui/component/watchlist"
	"github.com/hwyll/ticker/internal/ui/component/watchlist/row"

	util "github.com/hwyll/ticker/internal/ui/util"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

//nolint:gochecknoglobals
var (
	styleLogo  = util.NewStyle("#ffffd7", "#ff8700", true)
	styleGroup = util.NewStyle("#8a8a8a", "#303030", false)
	styleHelp  = util.NewStyle("#4e4e4e", "", true)
)

const (
	footerHeight = 1
)

// Model for UI
type Model struct {
	ctx                c.Context
	ready              bool
	headerHeight       int
	versionVector      int
	requestInterval    int
	assets             []c.Asset
	assetQuotes        []c.AssetQuote
	assetQuotesLookup  map[string]int
	positionSummary    asset.PositionSummary
	viewport           viewport.Model
	watchlist          *watchlist.Model
	summary            *summary.Model
	lastUpdateTime     string
	groupSelectedIndex int
	groupMaxIndex      int
	groupSelectedName  string
	currentSort        string
	monitors           *mon.Monitor
	mu                 sync.RWMutex
}

type tickMsg struct {
	versionVector int
}

type SetAssetQuoteMsg struct {
	symbol        string
	assetQuote    c.AssetQuote
	versionVector int
}

type SetAssetGroupQuoteMsg struct {
	assetGroupQuote c.AssetGroupQuote
	versionVector   int
}

// NewModel is the constructor for UI model
func NewModel(dep c.Dependencies, ctx c.Context, monitors *mon.Monitor) *Model {

	groupMaxIndex := len(ctx.Groups) - 1

	return &Model{
		ctx:               ctx,
		headerHeight:      getVerticalMargin(ctx.Config),
		ready:             false,
		requestInterval:   ctx.Config.RefreshInterval,
		versionVector:     0,
		assets:            make([]c.Asset, 0),
		assetQuotes:       make([]c.AssetQuote, 0),
		assetQuotesLookup: make(map[string]int),
		positionSummary:   asset.PositionSummary{},
		watchlist: watchlist.NewModel(watchlist.Config{
			Sort:                  ctx.Config.Sort,
			Separate:              ctx.Config.Separate,
			ShowPositions:         ctx.Config.ShowPositions,
			ExtraInfoExchange:     ctx.Config.ExtraInfoExchange,
			ExtraInfoFundamentals: ctx.Config.ExtraInfoFundamentals,
			Styles:                ctx.Reference.Styles,
		}),
		summary:            summary.NewModel(ctx),
		groupMaxIndex:      groupMaxIndex,
		groupSelectedIndex: 0,
		groupSelectedName:  "       ",
		currentSort:        ctx.Config.Sort,
		monitors:           monitors,
	}
}

// Init is the initialization hook for bubbletea
func (m *Model) Init() tea.Cmd {
	(*m.monitors).Start()

	// Start renderer and set symbols in parallel
	return tea.Batch(
		tick(0),
		func() tea.Msg {
			err := (*m.monitors).SetAssetGroup(m.ctx.Groups[m.groupSelectedIndex], m.versionVector)

			if m.ctx.Config.Debug && err != nil {
				m.ctx.Logger.Println(err)
			}

			return nil
		},
	)
}

// keyMatches checks if a key matches any binding
func keyMatches(key string, bindings []string) bool {
	for _, binding := range bindings {
		if key == binding {
			return true
		}
	}
	return false
}

// Update hook for bubbletea
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {

	case tea.KeyMsg:
		keyStr := msg.String()
		keyBindings := m.ctx.Config.KeyBindings
		
		switch {
		case keyMatches(keyStr, keyBindings.Quit):
			return m, tea.Quit		

		case keyMatches(keyStr, keyBindings.NextGroup), keyMatches(keyStr, keyBindings.PrevGroup):
			m.mu.Lock()

			groupSelectedCursor := -1
			if msg.String() == "tab" {
				groupSelectedCursor = 1
			}

			m.groupSelectedIndex = (m.groupSelectedIndex + groupSelectedCursor + m.groupMaxIndex + 1) % (m.groupMaxIndex + 1)

			// Invalidate all previous ticks, incremental price updates, and full price updates
			m.versionVector++

			m.mu.Unlock()

			// Set the new set of symbols in the monitors and initiate a request to refresh all price quotes
			// Eventually, SetAssetGroupQuoteMsg message will be sent with the new quotes once all of the HTTP request complete
			m.monitors.SetAssetGroup(m.ctx.Groups[m.groupSelectedIndex], m.versionVector) //nolint:errcheck

			return m, tickImmediate(m.versionVector)
			
		case keyMatches(keyStr, keyBindings.ScrollUp), keyMatches(keyStr, keyBindings.ScrollDown):
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd

		case keyMatches(keyStr, keyBindings.PageUp):
			m.viewport.PageUp()
			return m, nil

		case keyMatches(keyStr, keyBindings.PageDown):
			m.viewport.PageDown()
			return m, nil

		case keyMatches(keyStr, keyBindings.ChangeSort):
			m.mu.Lock()

			// Cycle through sort options: default -> alpha -> value -> user -> default
			sortOptions := []string{"", "alpha", "value", "user"}
			currentIndex := -1
			for i, sortOpt := range sortOptions {
				if m.currentSort == sortOpt {
					currentIndex = i

					break
				}
			}

			// Move to next sort option
			nextIndex := (currentIndex + 1) % len(sortOptions)
			m.currentSort = sortOptions[nextIndex]

			m.mu.Unlock()

			// Update watchlist component with new sort
			m.watchlist, cmd = m.watchlist.Update(watchlist.ChangeSortMsg(m.currentSort))

			return m, cmd
		}

	case tea.WindowSizeMsg:

		var cmd tea.Cmd

		m.mu.Lock()
		defer m.mu.Unlock()

		viewportHeight := msg.Height - m.headerHeight - footerHeight

		if !m.ready {
			m.viewport = viewport.New(msg.Width, viewportHeight)
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = viewportHeight
		}

		// Forward window size message to watchlist and summary component
		m.watchlist, cmd = m.watchlist.Update(msg)
		m.summary, _ = m.summary.Update(msg)

		return m, cmd

	// Trigger component re-render if data has changed
	case tickMsg:

		var cmd tea.Cmd
		cmds := make([]tea.Cmd, 0)

		m.mu.Lock()
		defer m.mu.Unlock()

		// Do not re-render if versionVector has changed and do not start a new timer with this versionVector
		if msg.versionVector != m.versionVector {
			return m, nil
		}

		// Update watchlist and summary components
		m.watchlist, cmd = m.watchlist.Update(watchlist.SetAssetsMsg(m.assets))
		m.summary, _ = m.summary.Update(summary.SetSummaryMsg(m.positionSummary))

		cmds = append(cmds, cmd)

		// Set the current tick time
		m.lastUpdateTime = getTime()

		// Update the viewport
		if m.ready {
			m.viewport, cmd = m.viewport.Update(msg)
			cmds = append(cmds, cmd)
		}

		cmds = append(cmds, tick(msg.versionVector))

		return m, tea.Batch(cmds...)

	case SetAssetGroupQuoteMsg:

		m.mu.Lock()
		defer m.mu.Unlock()

		// Do not update the assets and position summary if the versionVector has changed
		if msg.versionVector != m.versionVector {
			return m, nil
		}

		assets, positionSummary := asset.GetAssets(m.ctx, msg.assetGroupQuote)

		m.assets = assets
		m.positionSummary = positionSummary

		m.assetQuotes = msg.assetGroupQuote.AssetQuotes
		for i, assetQuote := range m.assetQuotes {
			m.assetQuotesLookup[assetQuote.Symbol] = i
		}

		m.groupSelectedName = m.ctx.Groups[m.groupSelectedIndex].Name

		return m, nil

	case SetAssetQuoteMsg:

		var i int
		var ok bool

		m.mu.Lock()
		defer m.mu.Unlock()

		if msg.versionVector != m.versionVector {
			return m, nil
		}

		// Check if this symbol is in the lookup
		if i, ok = m.assetQuotesLookup[msg.symbol]; !ok {
			return m, nil
		}

		// Check if the index is out of bounds
		if i >= len(m.assetQuotes) {
			return m, nil
		}

		// Check if the symbol is the same
		if m.assetQuotes[i].Symbol != msg.symbol {
			return m, nil
		}

		// Update the asset quote and generate a new position summary
		m.assetQuotes[i] = msg.assetQuote

		assetGroupQuote := c.AssetGroupQuote{
			AssetQuotes: m.assetQuotes,
			AssetGroup:  m.ctx.Groups[m.groupSelectedIndex],
		}

		assets, positionSummary := asset.GetAssets(m.ctx, assetGroupQuote)

		m.assets = assets
		m.positionSummary = positionSummary

		return m, nil

	case row.FrameMsg:
		var cmd tea.Cmd
		m.watchlist, cmd = m.watchlist.Update(msg)

		return m, cmd
	}

	return m, nil
}

// View rendering hook for bubbletea
func (m *Model) View() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.ready {
		return "\n  Initializing..."
	}

	m.viewport.SetContent(m.watchlist.View())

	viewSummary := ""

	if m.ctx.Config.ShowSummary && m.ctx.Config.ShowPositions {
		viewSummary += m.summary.View() + "\n"
	}

	return viewSummary +
		m.viewport.View() + "\n" +
		footer(m.viewport.Width, m.lastUpdateTime, m.groupSelectedName, m.currentSort, m.ctx.Config.KeyBindings)
}

func footer(width int, time string, groupSelectedName string, currentSort string, keyBindings c.KeyBindings) string {
	logoText := " ticker "

	// Early return for very narrow terminals
	if width < 80 {
		return styleLogo(logoText)
	}

	// Truncate long group names
	if len(groupSelectedName) > 12 {
		groupSelectedName = groupSelectedName[:12]
	}
	
	// Build footer components
	groupText := " " + groupSelectedName + " "
	sortDisplayName := getSortDisplayName(currentSort)
	helpText := buildHelpText(keyBindings, sortDisplayName)
	
	// Calculate minimum widths for responsive display
	groupMinWidth := len(logoText) + len(groupText) + len(time)
	helpMinWidth := groupMinWidth + len(helpText)
	
	// Center help text
	centeredHelpText := centerText(helpText, width - groupMinWidth)

	return grid.Render(grid.Grid{
		Rows: []grid.Row{
			{
				Width: width,
				Cells: []grid.Cell{
					{Text: styleLogo(logoText), Width: len(logoText)},
					{Text: styleGroup(groupText), Width: len(groupText), VisibleMinWidth: groupMinWidth},
					{Text: styleHelp(centeredHelpText), Width: len(centeredHelpText), VisibleMinWidth: helpMinWidth},
					{Text: styleHelp(time), Align: grid.Right},
				},
			},
		},
	})
}

// centerText centers text within a given width by adding left padding.
// If the text is already wider than or equal to totalWidth, it returns the text unchanged.
func centerText(text string, totalWidth int) string {
	textLen := len(text)
	if textLen >= totalWidth {
		return text
	}
	leftPad := (totalWidth - textLen) / 2
	return strings.Repeat(" ", leftPad) + text
}

// getSortDisplayName returns the display name for the current sort option
func getSortDisplayName(currentSort string) string {
	switch currentSort {
	case "alpha":
		return "alpha"
	case "value":
		return "value"
	case "user":
		return "user"
	default:
		return "change"
	}
}

// buildHelpText creates the help text from configured key bindings
func buildHelpText(keyBindings c.KeyBindings, sortDisplayName string) string {
	return fmt.Sprintf(" | %s: exit | %s: scroll-up | %s: scroll-down | %s: next-group | %s: prev-group | %s: sort-by (%s) | ",
		keyBindings.Quit[0],
		keyBindings.ScrollUp[0],
		keyBindings.ScrollDown[0],
		keyBindings.NextGroup[0],
		keyBindings.PrevGroup[0],
		keyBindings.ChangeSort[0],
		sortDisplayName,
	)
}

func getVerticalMargin(config c.Config) int {
	if config.ShowSummary && config.ShowPositions {
		return 2
	}

	return 0
}

// Send a new tick message with the versionVector 200ms from now
func tick(versionVector int) tea.Cmd {
	return tea.Tick(time.Second/5, func(time.Time) tea.Msg {
		return tickMsg{
			versionVector: versionVector,
		}
	})
}

// Send a new tick message immediately
func tickImmediate(versionVector int) tea.Cmd {

	return func() tea.Msg {
		return tickMsg{
			versionVector: versionVector,
		}
	}
}

func getTime() string {
	t := time.Now()

	return fmt.Sprintf("%s %02d:%02d:%02d", t.Weekday().String(), t.Hour(), t.Minute(), t.Second())
}
