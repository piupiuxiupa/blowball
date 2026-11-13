package executor

import "encoding/json"

var schemaBash = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell command to execute inside the sandboxed workspace."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`)
