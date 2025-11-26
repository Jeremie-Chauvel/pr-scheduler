# GitHub PR Auto-Merge Scheduler

A terminal UI tool to schedule GitHub pull requests for automatic merging at a specific time. Built with Go and [Charm's Bubble Tea](https://github.com/charmbracelet/bubbletea).

## Features

- 📋 List open pull requests in the current repository
- 🔍 Filter to show only your PRs
- ⏰ Schedule PRs to auto-merge at a specific time with preset options
- 🔔 Desktop notifications if a PR fails to merge
- 🎨 Beautiful terminal UI with Bubble Tea
- ✅ Automatic merge status verification

## Requirements

Before installing, ensure you have:

- **Go** 1.20 or higher
- **GitHub CLI (`gh`)** - [Installation guide](https://cli.github.com/manual/installation)
  - Must be authenticated: `gh auth login`
- **notify-send** (Linux only, for desktop notifications)

## Installation

### From Source

```bash
git clone git@github.com:Jeremie-Chauvel/pr-scheduler.git
cd pr-scheduler
go mod download
go build -o pr-scheduler main.go
mv ./pr-scheduler ~/.local/bin/
```

## Usage

1. Navigate to a Git repository with GitHub remote
2. Run the program: `pr-scheduler`

## Development

### Run Without Building

```bash
go run main.go
```

### Dependencies

All dependencies are managed via Go modules:

```bash
go mod tidy
```

## License

MIT
