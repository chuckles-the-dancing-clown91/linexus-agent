# linexus-agent
Edge Data Plane for the Linexus ecosystem — IoT telemetry, sensor providers, local SQLite cache, offline resilience. Runs on the physical Dignifundus hardware.

## Environment and tracking

Every other piece of state here is something the agent worked out for itself.
Two things are not: which environment this machine belongs to (`production`,
`staging`, `development`, …), and whether it is being **tracked** at all. Both
are decided upstream and pushed down as an `agent.environment` plan step —
what the Orchestrator emits for a `set_environment` intent dispatched from the
Hub.

```
Hub → Nexus → Orchestrator → agent.environment { environment, monitored, note }
                                       ↓
                              LocalCache::set_environment
```

**While `monitored` is false the agent stores nothing.**
`LocalCache::buffer_telemetry` returns `Ok(false)` and drops the reading. That
is deliberate and it is the whole point of the state living here rather than
only in a database upstream: an agent that does not know it is untracked keeps
shipping readings, and every one of them then has to be filtered out again on
the far side by somebody who remembers to. Silence is cheaper and more honest
than a filter.

Readings that *are* stored carry the environment they came from, so two samples
from the same fleet never end up inside the same average by accident.

The state is persisted in the SQLite cache rather than held in memory: a
machine muted on Friday must not come back tracked on Monday and page whoever
is on call.

Both fields degrade toward the **loud** option. A missing or empty environment
becomes `production`; anything that is not an explicit negative
(`false`/`0`/`no`/`off`) leaves the machine tracked. A malformed parameter must
never be the reason a production box goes quiet.
