# You.com Search Integration for mcpsnoop

This enhancement adds optional You.com web search capability to mcpsnoop, allowing developers to search the web directly from the MCP debugging TUI when investigating issues, errors, or tool behaviors.

## Features

- **Optional Integration**: Web search is completely optional - mcpsnoop works normally without it
- **Multiple Access Methods**: Search via keyboard shortcut `w`, command `:search <query>`, or command `:web <query>`
- **Environment Variable Support**: Set `YOUCOM_API_KEY` or use `--youcom-api-key` flag  
- **Configuration File Support**: Add `youcom-api-key = "your-key"` to `.mcpsnoop.toml`
- **Overlay Display**: Search results appear in a scrollable overlay, consistent with other mcpsnoop overlays
- **Error Handling**: Graceful fallback when API key is missing or requests fail

## Setup

### Option 1: Environment Variable (Recommended)
```bash
export YOUCOM_API_KEY="your-you.com-api-key"
mcpsnoop
```

### Option 2: Command Line Flag
```bash
mcpsnoop --youcom-api-key="your-you.com-api-key"
```

### Option 3: Configuration File
Create or edit `.mcpsnoop.toml` in your working directory:
```toml
youcom-api-key = "your-you.com-api-key"
```

## Usage

Once configured, you have several ways to search:

### Keyboard Shortcut
- Press `w` in the TUI to get a search prompt
- Enter your query and press Enter
- Results appear in an overlay

### Command Line
- Press `:` to open the command prompt
- Type `search <your query>` or `web <your query>`
- Press Enter to execute

### Examples
```
:search MCP server timeout error
:web "JSON-RPC 2.0" specification
:search python mcp client library
```

## Integration Points

The search feature is most useful when:

- **Debugging Errors**: Search for specific error messages or status codes you see in traces
- **Learning About Tools**: Research MCP server capabilities, tool specifications, or protocols  
- **Troubleshooting**: Look up common issues with specific MCP implementations
- **Documentation**: Find official specs, examples, or community discussions

## Implementation Details

- **API Endpoint**: Uses You.com search API for web search
- **Result Limit**: Returns top 5 results for clean TUI display
- **Timeout**: 10-second timeout on search requests
- **Privacy**: Search queries are sent to You.com; no local data is shared
- **Performance**: Asynchronous search doesn't block the TUI

## Help Text

The integration adds search to the help overlay (`?` key):

```
FRAME ACTIONS
w          search the web (You.com integration)
```

And to the hints bar when viewing MCP sessions.

## Error Handling

- **No API Key**: Shows helpful message about configuration
- **Network Issues**: Displays error message in TUI flash bar
- **API Errors**: Shows HTTP error details for debugging
- **Empty Results**: Indicates when no results are found

## Security Notes

- API keys are handled securely (not logged or traced)
- Search queries are transmitted to You.com over HTTPS
- No MCP session data is included in search requests
- You.com's privacy policy governs search data handling