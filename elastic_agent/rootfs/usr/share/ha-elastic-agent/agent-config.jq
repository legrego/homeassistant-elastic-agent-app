# Builds the standalone agent's configuration from the app options.
# Run with the options as input and these arguments:
#   $url $key $ca $insecure $pipeline $host
#
# Core gets its own input, so its multiline parser only sees Core's lines. Every
# other source shares one input, and turning a source off drops its events.

def enabled($name): if has($name) and .[$name] != null then .[$name] else true end;

(.tags // [] | map(select(. != null and . != ""))) as $tags
| (if (.read_existing_logs // false) then "head" else "tail" end) as $seek
| enabled("collect_core") as $core
| enabled("collect_supervisor") as $supervisor
| enabled("collect_apps") as $apps
| enabled("collect_host") as $host_logs

| def stream($id; $dataset):
    {
      id: $id,
      data_stream: {type: "logs", dataset: $dataset, namespace: "default"},
      seek: $seek
    }
    + (if ($tags | length) > 0 then {tags: $tags} else {} end);

  {
    outputs: {
      default: (
        {type: "elasticsearch", hosts: [$url], api_key: $key}
        + (if $insecure then {"ssl.verification_mode": "none"}
           elif $ca != "" then {"ssl.certificate_authorities": [$ca]}
           else {} end)
      )
    },
    # Monitoring would need an API key that can write the agent's own data
    # streams. The agent's logs are in the app's Log tab instead.
    agent: {monitoring: {enabled: false}},
    inputs: (
      [
        if $core then {
          id: "haos-core",
          type: "journald",
          use_output: "default",
          streams: [
            stream("haos-core"; "haos.core") + {
              include_matches: {match: ["CONTAINER_NAME=homeassistant"]},
              # Core writes one journal entry per line, so join every line that
              # doesn't start with a timestamp onto the one before it.
              parsers: [{
                multiline: {
                  type: "pattern",
                  pattern: "^(\\x1b\\[[0-9;]*m)?\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2}",
                  negate: true,
                  match: "after"
                }
              }]
            } + (if $pipeline != "" then {pipeline: $pipeline} else {} end)
          ]
        } else empty end,

        if ($supervisor or $apps or $host_logs) then {
          id: "haos-system",
          type: "journald",
          use_output: "default",
          streams: [
            stream("haos-system"; "haos.system") + {
              processors: (
                [{drop_event: {when: {equals: {"container.name": "homeassistant"}}}}]
                + (if $supervisor then [] else
                    [{drop_event: {when: {regexp: {"container.name": "^hassio_"}}}}] end)
                + (if $apps then [] else
                    [{drop_event: {when: {regexp: {"container.name": "^addon_"}}}}] end)
                + (if $host_logs then [] else
                    [{drop_event: {when: {not: {has_fields: ["container.name"]}}}}] end)
              )
            }
          ]
        } else empty end
      ]
    )
  }
