You are helping test an MCP server called ref. Use its tools to do the following, in order, and carry on to the next step even if one fails.

1. Call the echo tool with the text "hello".
2. Call the confirm_action tool with the action "delete the demo file".
3. Call the slow_task tool with seconds set to 3.
4. Read the resource ref://about.
5. Call the list_roots tool.
6. Call the unlock_tool tool, then call the bonus_tool tool it makes available.
7. In a single step, call slow_task with seconds set to 2 and echo with the text "parallel" at the same time, without waiting for either to finish.
8. In a single step, call slow_read with seconds set to 2 and echo_read with the text "parallel" at the same time, without waiting for either to finish.
9. Call the echo tool with the text "done".

Then reply with one short line per step saying what happened.
