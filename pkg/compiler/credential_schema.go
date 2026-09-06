package compiler

import (
	"sync"

	"github.com/wklken/apisix-go/pkg/util"
)

// APISIX 3.17 schema_def.credential governs the resource envelope separately
// from the selected authentication plugin's consumer schema.
const credentialEnvelopeSchema = `{
 "type":"object",
 "properties":{
  "id":{"oneOf":[
   {"anyOf":[
    {"type":"string","minLength":1,"maxLength":64,"pattern":"^[a-zA-Z0-9_.-]+$"},
    {"type":"integer","minimum":1}
   ]},
   {"type":"string","minLength":15,"maxLength":128,"pattern":"^[a-zA-Z0-9_-]+/credentials/[a-zA-Z0-9_.-]+$"}
  ]},
  "name":{"type":"string","minLength":1,"maxLength":256},
  "desc":{"type":"string","maxLength":256},
  "labels":{"type":"object","additionalProperties":{"type":"string","maxLength":256,"pattern":"^\\S+$"}},
  "create_time":{"type":"integer"},
  "update_time":{"type":"integer"},
  "plugins":{"type":"object","maxProperties":1}
 },
 "additionalProperties":false
}`

var compiledCredentialEnvelope = sync.OnceValues(func() (*util.CompiledSchema, error) {
	return util.CompileSchema(credentialEnvelopeSchema)
})

func validCredentialEnvelope(document map[string]any) bool {
	schema, err := compiledCredentialEnvelope()
	return err == nil && schema.Validate(document) == nil
}
