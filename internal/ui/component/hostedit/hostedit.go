// Package hostedit contains UI components for editing host model attributes.
package hostedit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samber/lo"

	"github.com/grafviktor/goto/internal/model"
	"github.com/grafviktor/goto/internal/state"
	"github.com/grafviktor/goto/internal/storage"
	"github.com/grafviktor/goto/internal/ui/component/hostlist"
	"github.com/grafviktor/goto/internal/ui/component/input"
	"github.com/grafviktor/goto/internal/ui/message"
	"github.com/grafviktor/goto/internal/utils"
)

// Size struct is used by terminal resize event.
type Size struct {
	Width  int
	Height int
}

type (
	// MsgClose triggers when users exits from edit form without saving results.
	MsgClose struct{}
	// MsgSave triggers when users saves results.
	MsgSave struct{}
	// debouncedMessage is used to trigger side-effects. For instance dispatch RunProcessLoadSSHConfig
	// which reads host config from ~/.ssh/config file.
	debouncedMessage struct {
		wrappedMsg  tea.Msg
		debounceTag int
	}
)

const (
	inputTitle int = iota
	inputAddress
	inputDescription
	inputLogin
	inputNetworkPort
	inputIdentityFile
)

var (
	// ItemID is a key to extract item id from application context.
	ItemID       = struct{}{}
	defaultTitle = "host details"
	debounceTime = time.Millisecond * 500
)

type iLogger interface {
	Debug(format string, args ...any)
	Info(format string, args ...any)
}

func notEmptyValidator(s string) error {
	if utils.StringEmpty(s) {
		return fmt.Errorf("value is required")
	}

	return nil
}

func networkPortValidator(s string) error {
	if utils.StringEmpty(s) {
		return nil
	}

	auto := 0 // 0 is used to autodetect base, see strconv.ParseUint
	maxLengthBit := 16
	if num, err := strconv.ParseUint(s, auto, maxLengthBit); err != nil || num < 1 {
		return fmt.Errorf("network port must be a number which is less than 65,535")
	}

	return nil
}

func getKeyMap(focusedInput int) keyMap {
	if focusedInput == inputTitle || focusedInput == inputAddress {
		keys.CopyInputValue.SetEnabled(true)
	} else {
		keys.CopyInputValue.SetEnabled(false)
	}

	return keys
}

type editModel struct {
	appState     *state.ApplicationState
	focusedInput int
	help         help.Model
	host         model.Host
	hostStorage  storage.HostStorage
	inputs       []input.Input
	isNewHost    bool
	keyMap       keyMap
	logger       iLogger
	ready        bool
	title        string
	viewport     viewport.Model
	debounceTag  int
}

// New - returns new edit host form.
func New(ctx context.Context, storage storage.HostStorage, state *state.ApplicationState, log iLogger) *editModel {
	initialFocusedInput := inputTitle

	// If we can't cast host id to int, that means we're adding a new host. Ignore the error
	hostID, _ := ctx.Value(ItemID).(int)
	host, hostNotFoundErr := storage.Get(hostID)
	if hostNotFoundErr != nil {
		// Logger should notify that this is a new host
		host = model.Host{}
	}

	m := editModel{
		inputs:       make([]input.Input, 6),
		hostStorage:  storage,
		host:         host,
		help:         help.New(),
		keyMap:       getKeyMap(initialFocusedInput),
		appState:     state,
		logger:       log,
		focusedInput: initialFocusedInput,
		title:        defaultTitle,
		// This variable is for optimization. By introducing it, we can avoid unnecessary database reads
		// every time we change values which depend on each other, for instance: "Title" and "Address".
		// Use text search and see where 'isNewHost' is used.
		isNewHost: hostNotFoundErr != nil,
	}

	var t input.Input
	for i := range m.inputs {
		t = *input.New()
		t.Cursor.Style = cursorStyle

		switch i {
		case inputTitle:
			t.SetLabel("Title")
			t.SetValue(host.Title)
			t.Placeholder = "*required*" //nolint:goconst
			t.Validate = notEmptyValidator
		case inputAddress:
			t.SetLabel("Host")
			t.CharLimit = 128
			t.SetValue(host.Address)
			t.Placeholder = "*required*"
			t.Validate = notEmptyValidator
		case inputDescription:
			t.SetLabel("Description")
			t.CharLimit = 512
			t.Placeholder = "n/a"
			t.SetValue(host.Description)
		case inputLogin:
			t.SetLabel("Login")
			t.CharLimit = 128
			t.Placeholder = fmt.Sprintf("default: %s", m.appState.HostSSHConfig.User)
			t.SetValue(host.LoginName)
		case inputNetworkPort:
			t.SetLabel("Network port")
			t.CharLimit = 5
			t.Placeholder = fmt.Sprintf("default: %s", m.appState.HostSSHConfig.Port)
			t.SetValue(host.RemotePort)
			t.Validate = networkPortValidator
		case inputIdentityFile:
			t.SetLabel("Identity file")
			t.CharLimit = 512
			t.Placeholder = fmt.Sprintf("default: %s", m.appState.HostSSHConfig.IdentityFile)
			t.SetValue(host.PrivateKeyPath)
		}

		m.inputs[i] = t
	}

	m.inputs[m.focusedInput].Focus()

	return &m
}

func (m *editModel) Init() tea.Cmd { return nil }

func (m *editModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// This message never comes through automatically on Windows OS, we send it from init_win.go.
		m.updateViewPort(msg)
	case tea.KeyMsg:
		cmd = m.handleKeyboardEvent(msg)
		m.viewport.SetContent(m.inputsView())
	case debouncedMessage:
		cmd = m.handleDebouncedMessage(msg)
	case message.HostSSHConfigLoaded:
		m.updateInputPlaceholders()
		m.updateInputTitles()
		m.viewport.SetContent(m.inputsView())
	}

	return m, cmd
}

func (m *editModel) View() string {
	if !m.ready {
		// Create viewport, ideally this call should be located in init function,
		// but this function does not trigger for child components
		m.updateViewPort(nil)
	}

	viewPortContent := m.viewport.View()
	return fmt.Sprintf("%s\n%s\n%s", m.headerView(), viewPortContent, m.helpView())
}

func (m *editModel) handleKeyboardEvent(msg tea.KeyMsg) tea.Cmd {
	// If title displays an error, due to an incorrect title for instance
	// once user presses any button, we should reset it to default value
	m.title = defaultTitle

	switch {
	case key.Matches(msg, m.keyMap.Save):
		m.logger.Info("[UI] Save changes for host id: %v", m.host.ID)
		return m.save(msg)
	case key.Matches(msg, m.keyMap.CopyInputValue):
		m.handleCopyInputValueShortcut()
		return nil
	case key.Matches(msg, m.keyMap.Down) || key.Matches(msg, m.keyMap.Up):
		return m.inputFocusChange(msg)
	case key.Matches(msg, m.keyMap.Discard):
		m.logger.Info("[UI] Discard changes for host id: %v", m.host.ID)
		return message.TeaCmd(MsgClose{})
	default:
		// Handle all other key events
		return m.focusedInputProcessKeyEvent(msg)
	}
}

func (m *editModel) handleDebouncedMessage(msg debouncedMessage) tea.Cmd {
	// This function debounces a tea.Message. In order to find the last message from a list of duplicate messages
	// debounceTag is used. Every time a tea.Tick message is dispatched, debounceTag is incremented. Then, when
	// tea.Tick message triggers by timer (by debounceTime) it compares its own debounceTag with the model's
	// debounceTag and only triggers when they're equal. That guarantees that only last message will be handled.
	m.debounceTag++

	return tea.Tick(debounceTime, func(_ time.Time) tea.Msg {
		// Need to decrement the model's debounce tag before comparing. This simply relates to order of operations.
		if msg.debounceTag == m.debounceTag-1 {
			// Only the last message from messages dispatched within a certain interval will be handled.
			return msg.wrappedMsg
		}

		return nil
	})
}

func (m *editModel) save(_ tea.Msg) tea.Cmd {
	for i := range m.inputs {
		if m.inputs[i].Validate != nil {
			if err := m.inputs[i].Validate(m.inputs[i].Value()); err != nil {
				m.logger.Info(
					"[UI] Cannot save host with id %v. Reason: '%s' is not valid, %s",
					m.host.ID,
					m.inputs[i].Label,
					err.Error(),
				)
				m.inputs[i].Err = err
				m.title = fmt.Sprintf("%s is not valid", m.inputs[i].Label)

				return nil
			}
		}

		switch i {
		case inputTitle:
			m.host.Title = m.inputs[i].Value()
		case inputAddress:
			m.host.Address = m.inputs[i].Value()
		case inputDescription:
			m.host.Description = m.inputs[i].Value()
		case inputLogin:
			m.host.LoginName = m.inputs[i].Value()
		case inputNetworkPort:
			m.host.RemotePort = m.inputs[i].Value()
		case inputIdentityFile:
			m.host.PrivateKeyPath = m.inputs[i].Value()
		}
	}

	host, _ := m.hostStorage.Save(m.host)
	// Need to check storage error and update application status:
	// if err !=nil { return message.TeaCmd(message.Error{Err: err}) }
	// or
	// m.title = err

	return tea.Sequence(
		message.TeaCmd(MsgClose{}),
		// Order matters here! That's why we use tea.Sequence instead of tea.Batch.
		// 'HostListSelectItem' message should be dispatched
		// before 'MsgRefreshRepo'. The reasons of that is because
		// 'MsgRefreshRepo' handler automatically sets focus on previously selected item.
		message.TeaCmd(message.HostListSelectItem{HostID: host.ID}),
		message.TeaCmd(hostlist.MsgRefreshRepo{}),
	)
}

func (m *editModel) copyInputValueFromTo(sourceInput, destinationInput int) {
	newValue := m.inputs[sourceInput].Value()

	// Temporary remove input validator.
	// It's necessary, because input.SetValue(...) invokes Validate function,
	// if the input contains invalid value, Validate function returns error and
	// rejects new value. That leads to a problem - when user removes all symbols
	// from address input, title input still preserves the very last letter.
	// A better way would be to use own validation logic instead of relying
	// on input.Validate.
	validator := m.inputs[destinationInput].Validate
	m.inputs[destinationInput].Validate = nil
	m.inputs[destinationInput].SetValue(newValue)
	m.inputs[destinationInput].SetCursor(len(newValue))
	m.inputs[destinationInput].Validate = validator
	m.inputs[destinationInput].Err = m.inputs[destinationInput].Validate(newValue)
	m.logger.Debug(
		"[UI] Copy '%s' value to '%s', new value = %s",
		m.inputs[sourceInput].Label,
		m.inputs[destinationInput].Label,
		newValue,
	)
}

func (m editModel) focusedInputProcessKeyEvent(msg tea.Msg) tea.Cmd {
	var shouldUpdateTitle bool
	previousValue := m.inputs[m.focusedInput].Value()

	// Decide if we need to propagate hostname to title.
	// Note, that we should make this decision BEFORE updating focused input
	if m.focusedInput == inputTitle {
		addressEqualsTitle := m.inputs[inputTitle].Value() == m.inputs[inputAddress].Value()

		// If there wouldn't be 'm.isNewHost' variable we would have to query database for every key event
		if m.isNewHost && addressEqualsTitle {
			// If host doesn't exist in the repo and title equals address
			// we should copy text from address to title.
			shouldUpdateTitle = true
		}
	}

	// Update focused input
	m.inputs[m.focusedInput].Update(msg)

	// Then, update title if we should
	if shouldUpdateTitle {
		m.copyInputValueFromTo(inputTitle, inputAddress)
	}

	// If type in address field
	if m.focusedInput == inputAddress {
		currentValue := m.inputs[inputAddress].Value()

		// And value changed
		if previousValue != currentValue {
			// Load SSH config for the specified hostname
			return message.TeaCmd(debouncedMessage{
				wrappedMsg:  message.RunProcessLoadSSHConfig{SSHConfigHostname: currentValue},
				debounceTag: m.debounceTag, // See the comments in debouncedMessage definition.
			})
		}
	}

	return nil
}

func (m *editModel) updateViewPort(msg tea.Msg) {
	headerHeight := lipgloss.Height(m.headerView())
	helpMenuHeight := lipgloss.Height(m.helpView())

	if !m.ready {
		m.ready = true
		m.viewport = viewport.New(m.appState.Width, m.appState.Height-headerHeight-helpMenuHeight)
		m.viewport.SetContent(m.inputsView())
	} else if resizeMsg, ok := msg.(tea.WindowSizeMsg); ok {
		m.viewport.Width = resizeMsg.Width
		m.viewport.Height = resizeMsg.Height - headerHeight - helpMenuHeight
		m.logger.Debug("[UI] Set edit host viewport size: %d %d", m.viewport.Width, m.viewport.Height)
	}
}

func (m *editModel) inputFocusChange(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	keyMsg := msg.(tea.KeyMsg)

	minFocusIndex := 0
	maxFocusIndex := len(m.inputs) - 1
	inputHeight := 0

	if len(m.inputs) > 0 {
		// Control viewport manually because height of input element is greater than one
		// therefore, we need to scroll several lines at once instead of just a single line.
		// Normally we don't need to handle scroll events, other than forward app messages to
		// the viewport: m.viewport, cmd = m.viewport.Update(msg)
		inputHeight = lipgloss.Height(m.inputsView()) / len(m.inputs)
	}

	// Update index of the focused element
	if key.Matches(keyMsg, m.keyMap.Up) && m.focusedInput > minFocusIndex { //nolint:gocritic // it's better without switch
		m.focusedInput--
		m.viewport.LineUp(inputHeight)
	} else if key.Matches(keyMsg, m.keyMap.Down) && m.focusedInput < maxFocusIndex {
		m.focusedInput++
		m.viewport.LineDown(inputHeight)
	} else {
		return nil
	}

	// Should be extracted to "Validate" function
	for i := 0; i <= len(m.inputs)-1; i++ {
		if m.inputs[i].Validate != nil {
			m.inputs[i].Err = m.inputs[i].Validate(m.inputs[i].Value())
			m.logger.Debug("[UI] Input '%s' is valid: %t", m.inputs[i].Label, m.inputs[i].Err == nil)
		}

		if i == m.focusedInput {
			// KeyMap depends on focused input - when address is focused, we allow
			// a user to copy address value to title.
			m.keyMap = getKeyMap(i)
			m.logger.Debug("[UI] Focus input: '%s'", m.inputs[i].Label)

			// Set focused state
			cmds = append(cmds, m.inputs[i].Focus())
		} else {
			// Remove focused state
			m.inputs[i].Blur()
		}
	}

	return tea.Batch(cmds...)
}

func (m *editModel) handleCopyInputValueShortcut() {
	// Allow a user to copy values between address and title,
	// because the chances are that these two inputs will have
	// the same values.
	if m.focusedInput == inputTitle {
		m.copyInputValueFromTo(m.focusedInput, inputAddress)
	} else if m.focusedInput == inputAddress {
		m.copyInputValueFromTo(m.focusedInput, inputTitle)
	}
}

// This function should probably be moved to model.
func (m *editModel) isCustomConnectString() bool {
	rawValue := strings.TrimSpace(m.inputs[inputAddress].Value())
	containsSpace := strings.Contains(rawValue, " ")
	containsAtSymbol := strings.Contains(rawValue, "@")

	return containsSpace || containsAtSymbol
}

// func (m *editModel) updateInputPlaceholders() {
// 	m.logger.Debug("[UI] Take input placeholders from selected host SSH config")
// 	prefix := lo.Ternary(m.isCustomConnectString(), "disabled", "default")
// 	m.inputs[inputLogin].Placeholder = fmt.Sprintf("%s: %s", prefix, m.appState.HostSSHConfig.User)
// 	m.inputs[inputNetworkPort].Placeholder = fmt.Sprintf("%s: %s", prefix, m.appState.HostSSHConfig.Port)
// 	m.inputs[inputIdentityFile].Placeholder = fmt.Sprintf("%s: %s", prefix, m.appState.HostSSHConfig.IdentityFile)
// }

func (m *editModel) updateInputPlaceholders() {
	m.logger.Debug("[UI] Take input placeholders from selected host SSH config")
	// prefix := "default: "
	prefix := lo.Ternary(m.isCustomConnectString(), "readonly: ", "default: ")
	m.inputs[inputLogin].Placeholder = fmt.Sprintf("%s%s", prefix, m.appState.HostSSHConfig.User)
	m.inputs[inputNetworkPort].Placeholder = fmt.Sprintf("%s%s", prefix, m.appState.HostSSHConfig.Port)
	m.inputs[inputIdentityFile].Placeholder = fmt.Sprintf("%s%s", prefix, m.appState.HostSSHConfig.IdentityFile)
}

func (m *editModel) updateInputTitles() {
	m.logger.Debug("[UI] Update input titles")
	shouldDisableSSHArguments := !m.isCustomConnectString()

	inputsToProcess := []*input.Input{
		&m.inputs[inputLogin],
		&m.inputs[inputNetworkPort],
		&m.inputs[inputIdentityFile],
	}

	if m.isCustomConnectString() {
		lo.ForEach(inputsToProcess, func(i *input.Input, n int) {
			i.SetEnabled(shouldDisableSSHArguments)
		})
	}
}

func (m *editModel) inputsView() string {
	var b strings.Builder
	for i := range m.inputs {
		input := m.inputs[i]

		b.WriteString(input.View())
		if i < len(m.inputs) {
			b.WriteString("\n\n")
		}
	}

	return docStyle.Render(b.String())
}

func (m *editModel) headerView() string {
	return titleStyle.Render(m.title)
}

func (m *editModel) helpView() string {
	return menuStyle.Render(m.help.View(m.keyMap))
}
