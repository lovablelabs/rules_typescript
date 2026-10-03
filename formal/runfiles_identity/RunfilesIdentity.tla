-------------------------- MODULE RunfilesIdentity --------------------------
CONSTANT CheckIdentity
VARIABLES canonicalRelation, canonicalAlias, renderer, phase

vars == <<canonicalRelation, canonicalAlias, renderer, phase>>

CanonicalEntries ==
    CASE canonicalRelation = "single" -> [coordinate \in {"first"} |-> "a"]
      [] canonicalRelation = "shared" -> [coordinate \in {"first", "second"} |-> "a"]
      [] OTHER -> [coordinate \in {"first", "second"} |-> IF coordinate = "first" THEN "a" ELSE "b"]
CanonicalUnique == \A left, right \in DOMAIN CanonicalEntries:
    CanonicalEntries[left] = CanonicalEntries[right] => left = right
Admitted == ~CheckIdentity \/ CanonicalUnique

Init ==
    /\ canonicalRelation \in {"single", "shared", "distinct"}
    /\ canonicalAlias \in BOOLEAN /\ renderer \in {"stage", "build"}
    /\ phase = "assembling"
Publish ==
    /\ phase = "assembling"
    /\ phase' = IF Admitted THEN "published" ELSE "rejected"
    /\ UNCHANGED <<canonicalRelation, canonicalAlias, renderer>>
Next == Publish \/ (phase # "assembling" /\ UNCHANGED vars)
Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ canonicalRelation \in {"single", "shared", "distinct"}
    /\ canonicalAlias \in BOOLEAN /\ renderer \in {"stage", "build"}
    /\ phase \in {"assembling", "published", "rejected"}
DuplicateCanonicalOwnersReject ==
    phase # "assembling" /\ ~CanonicalUnique => phase = "rejected"
UniqueCanonicalOwnersPublish ==
    phase # "assembling" /\ CanonicalUnique => phase = "published"

=============================================================================
