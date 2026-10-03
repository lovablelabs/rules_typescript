-------------------------- MODULE CanonicalTrace --------------------------
EXTENDS RunfilesIdentity
CONSTANTS Relation, Alias, Renderer

TraceInit == Init /\ canonicalRelation = Relation /\ canonicalAlias = Alias /\ renderer = Renderer
TraceSpec == TraceInit /\ [][Next]_vars
TracePending == phase = "assembling"
ReplayState == [phase |-> phase, renderer |-> renderer,
    bindings |-> {[coordinate |-> key, artifact |-> CanonicalEntries[key], canonical |-> TRUE]: key \in DOMAIN CanonicalEntries}
                 \cup (IF canonicalAlias THEN {[coordinate |-> "alias", artifact |-> "a", canonical |-> FALSE]} ELSE {})]
=============================================================================
