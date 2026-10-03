------------------------- MODULE ProducerPublication -------------------------
EXTENDS Naturals, Sequences, FiniteSets

CONSTANT Seed
VARIABLES caseName, phase, observed

Cases == {"module_data", "module_data_moved", "source_data", "shared_source_owners", "json_data",
          "json_srcs", "asset_data", "asset_data_moved", "asset_member", "js_srcs",
          "asset_js_srcs", "alias_data", "composed_alias_data", "alias_conflict",
          "alias_cycle", "origin_conflict", "repeated_origin", "opaque_data",
          "opaque_data_moved", "scoped_data", "shadowed_data",
          "identity_root_node", "emitted_root_node", "identity_root_vitest", "emitted_root_vitest", "opaque_root_vitest", "alias_root_subset_vitest"}
RootCases == {"identity_root_node", "emitted_root_node", "identity_root_vitest", "emitted_root_vitest", "opaque_root_vitest", "alias_root_subset_vitest"}
IdentityRootCases == {"identity_root_node", "identity_root_vitest"}
MovedCases == {"module_data_moved", "asset_data_moved", "opaque_data_moved"}
AssetCases == {"asset_data", "asset_data_moved", "asset_member"}
AliasCases == {"alias_data", "composed_alias_data", "alias_conflict", "alias_cycle", "alias_root_subset_vitest"}
OpaqueCases == {"opaque_data", "opaque_data_moved", "opaque_root_vitest"}
ScopedCases == {"scoped_data", "shadowed_data"}
SrcCases == {"json_srcs", "js_srcs", "asset_js_srcs"} \cup RootCases

Prefix(left, right) == Len(left) <= Len(right) /\ SubSeq(right, 1, Len(left)) = left
Relative(parent, path) == IF Prefix(parent, path) THEN SubSeq(path, Len(parent) + 1, Len(path)) ELSE path
Base(name) == <<"producer_cases", name>>
Kind(name) == CASE name \in {"source_data", "shared_source_owners"} -> "source"
               [] name \in {"json_data", "json_srcs"} -> "json"
               [] name \in AssetCases -> "asset"
               [] name = "asset_js_srcs" -> "asset_js"
               [] name \in IdentityRootCases -> "generated_js"
               [] name \in OpaqueCases -> "opaque"
               [] OTHER -> "module"
RuntimeIdentity(name) == IF Kind(name) \in {"source", "generated_js"} THEN "source" ELSE "runtime"
ProducerPackage(name) == Base(name) \o <<IF name \in AssetCases THEN "lib" ELSE "p">>
ConsumerPackage(name) == IF name \in AssetCases \cup AliasCases THEN Base(name) \o <<"app">> ELSE ProducerPackage(name)
SourceRelative(name) == CASE Kind(name) = "json" -> <<"value.json">>
                         [] Kind(name) = "asset" -> <<"asset.txt">>
                         [] Kind(name) = "asset_js" -> <<"asset.js">>
                         [] name \in IdentityRootCases -> <<"entry.test.js">>
                         [] name \in RootCases -> <<"entry.test.ts">>
                         [] name \in ScopedCases -> <<"nested", "entry.ts">>
                         [] OTHER -> <<"entry.ts">>
RuntimeRelative(name) == IF name \in RootCases THEN <<"entry.test.js">>
                        ELSE IF Kind(name) \in {"module", "opaque"}
                        THEN Append(SubSeq(SourceRelative(name), 1, Len(SourceRelative(name)) - 1), "entry.js")
                        ELSE SourceRelative(name)
AliasFacts(name) == CASE name = "composed_alias_data" -> {<<"alias", "intermediate">>, <<"intermediate", "runtime">>}
                     [] name = "alias_root_subset_vitest" -> {<<"alias", "runtime">>, <<"other_alias", "runtime">>}
                     [] name = "alias_conflict" -> {<<"alias", "runtime">>, <<"alias", "other_runtime">>}
                     [] name = "alias_cycle" -> {<<"alias", "intermediate">>, <<"intermediate", "alias">>}
                     [] name = "alias_data" -> {<<"alias", "runtime">>}
                     [] OTHER -> {}
SourceFacts(name) == IF Kind(name) = "opaque" THEN {}
                    ELSE {<<"source", RuntimeIdentity(name), "producer">>} \cup
                         (IF name = "origin_conflict" THEN {<<"other_source", "runtime", "producer">>} ELSE {})
                         \cup (IF name = "shared_source_owners" THEN {<<"source", "source", "peer">>} ELSE {})
Input(name) == [name |-> name, kind |-> Kind(name), runtime_id |-> RuntimeIdentity(name),
    producer_package |-> ProducerPackage(name), consumer_package |-> ConsumerPackage(name),
    source_path |-> ProducerPackage(name) \o SourceRelative(name),
    runtime_path |-> ProducerPackage(name) \o RuntimeRelative(name),
    request |-> IF AliasFacts(name) = {} THEN RuntimeIdentity(name) ELSE "alias",
    request_path |-> IF AliasFacts(name) = {} THEN ProducerPackage(name) \o RuntimeRelative(name)
                    ELSE ProducerPackage(name) \o <<"links", "alias.test.js">>,
    edge |-> IF name \in SrcCases THEN "srcs" ELSE "data",
    foreign_sources |-> IF name \in MovedCases THEN {Base(name) \o <<"q", "config.json">>} ELSE {},
    aliases |-> AliasFacts(name), origins |-> SourceFacts(name),
    producer_files |-> CASE Kind(name) = "opaque" -> {"source", "declaration"}
                       [] Kind(name) = "generated_js" -> {} [] OTHER -> {"source"},
    producer_js |-> IF Kind(name) \in {"module", "opaque", "generated_js"} THEN {RuntimeIdentity(name)} ELSE {},
    runner |-> CASE name \in {"identity_root_node", "emitted_root_node"} -> "node_test"
                [] name \in {"identity_root_vitest", "emitted_root_vitest", "opaque_root_vitest", "alias_root_subset_vitest"} -> "vitest" [] OTHER -> "",
    local_path |-> IF name \in RootCases THEN ConsumerPackage(name) \o <<"local.test.js">> ELSE <<>>,
    roots |-> IF name \in RootCases THEN {IF AliasFacts(name) = {} THEN RuntimeIdentity(name) ELSE "alias", "local_source"} ELSE {},
    extra_inputs |-> IF name = "alias_root_subset_vitest" THEN {"other_alias"} ELSE {},
    local_origins |-> IF name \in RootCases THEN {<<"local_source", "local_runtime", "test">>} ELSE {},
    scope |-> name \in ScopedCases, shadow |-> name = "shadowed_data", member |-> name = "asset_member",
    repeat |-> name = "repeated_origin", peer |-> name = "shared_source_owners",
    injected |-> AliasFacts(name) # {} \/ name = "origin_conflict"]

RECURSIVE Resolve(_, _, _)
Resolve(file, aliases, seen) ==
    LET targets == {pair[2]: pair \in {pair \in aliases: pair[1] = file}}
    IN IF file \in seen THEN "cycle"
       ELSE IF targets = {} THEN file
       ELSE IF Cardinality(targets) # 1 THEN "conflict"
       ELSE Resolve(CHOOSE target \in targets: TRUE, aliases, seen \cup {file})
OriginUnique(facts) == \A left, right \in facts:
    left[2] = right[2] => left[1] = right[1]
Failure(input) == CASE Resolve(input.request, input.aliases, {}) = "cycle" -> "runtime aliases contain a cycle"
                  [] Resolve(input.request, input.aliases, {}) = "conflict" -> "conflicting canonical Files"
                  [] ~OriginUnique(input.origins) -> "has conflicting source Files"
                  [] input.shadow -> "is shadowed by runtime manifest"
                  [] OTHER -> ""
LayoutRoot(input) ==
    LET paths == input.foreign_sources \cup (IF input.origins = {} THEN {} ELSE {input.source_path})
        roots == {SubSeq(input.consumer_package, 1, length): length \in 0..Len(input.consumer_package)}
        shared == {root \in roots: \A path \in paths: Prefix(root, SubSeq(path, 1, Len(path) - 1))}
    IN CHOOSE root \in shared: \A other \in shared: Len(other) <= Len(root)
Placement(input, coordinate) == IF input.foreign_sources = {} THEN coordinate
                              ELSE input.consumer_package \o Relative(LayoutRoot(input), coordinate)
DataCoordinate(input) == input.consumer_package \o Relative(input.consumer_package, input.request_path)
View(path, kind, coordinate) == [path |-> path, kind |-> kind, coordinate |-> coordinate]
RequiredViews(input) ==
    {View(input.runtime_path, "canonical", <<>>)}
    \cup (IF input.edge = "data"
          THEN {View(Placement(input, DataCoordinate(input)),
                     IF input.kind = "asset" THEN "asset" ELSE "alias",
                     IF input.kind = "asset" THEN input.runtime_path ELSE DataCoordinate(input))}
          ELSE {})
    \cup (IF input.foreign_sources # {} /\ input.kind # "opaque"
          THEN {View(Placement(input, input.runtime_path),
                     IF input.kind = "asset" THEN "asset" ELSE "alias", input.runtime_path)}
          ELSE {})
ProducerOwners(input) == {"producer"} \cup {origin[3]: origin \in input.origins}
RequiredOrigins(input) == IF input.kind \in {"asset", "opaque"} THEN {} ELSE input.origins
DirectData(input) == {view.path: view \in {view \in RequiredViews(input): view.kind # "canonical"}}
    \cup (IF input.kind = "json" /\ input.edge = "srcs" THEN {input.runtime_path} ELSE {})
DirectJS(input) == IF input.edge = "srcs" /\ input.kind \in {"module", "asset_js", "generated_js", "opaque"} THEN {input.runtime_path} ELSE {}
RootFacts(input) == input.origins \cup input.local_origins
SelectedRoots(input) ==
    {pair \in input.roots \X ({input.runtime_id} \cup {origin[2]: origin \in RootFacts(input)}):
        Resolve(pair[1], input.aliases, {}) = pair[2] \/
        \E origin \in RootFacts(input): origin[1] = Resolve(pair[1], input.aliases, {}) /\ origin[2] = pair[2]}
RunnerOrigins(input) ==
    {<<origin[1], origin[2]>>: origin \in {origin \in RootFacts(input): origin[2] \in {pair[2]: pair \in SelectedRoots(input)}}}
EmptyObservation == [error |-> "", live |-> {}, owners |-> {}, origins |-> {}, views |-> {},
                     direct_data |-> {}, direct_js |-> {}, copyable |-> {}, selected |-> {}, discovery |-> {}, runner_origins |-> {}]
CorrectProjection(input) == IF Failure(input) # ""
                     THEN [EmptyObservation EXCEPT !.error = Failure(input)]
                     ELSE [error |-> "", live |-> {input.runtime_id}, owners |-> ProducerOwners(input),
                           origins |-> RequiredOrigins(input), views |-> RequiredViews(input),
                           direct_data |-> DirectData(input), direct_js |-> DirectJS(input),
                           copyable |-> IF input.member THEN DirectData(input) ELSE {},
                           selected |-> SelectedRoots(input), discovery |-> SelectedRoots(input), runner_origins |-> RunnerOrigins(input)]
Projection(input) == CorrectProjection(input)

ReinterpretPublication(input) ==
    LET result == CorrectProjection(input)
    IN IF result.error = "" /\ input.kind \notin {"asset", "opaque"}
       THEN [result EXCEPT !.origins = {<<"runtime", "runtime", "consumer">>}] ELSE result
DropExplicitAsset(input) ==
    LET result == CorrectProjection(input)
    IN IF result.error = "" /\ input.kind = "asset"
       THEN [result EXCEPT !.views = {view \in @: view.path # Placement(input, DataCoordinate(input))}] ELSE result
RejectRepeatedPlacement(input) ==
    LET result == CorrectProjection(input)
    IN IF result.error = "" /\ input.foreign_sources # {} /\ input.kind = "module"
       THEN [EmptyObservation EXCEPT !.error = "already occupied"] ELSE result
AcceptConflicts(input) ==
    IF Failure(input) # "" THEN [CorrectProjection(input) EXCEPT !.error = ""] ELSE CorrectProjection(input)
InferOpaqueOrigin(input) ==
    LET result == CorrectProjection(input)
    IN IF result.error = "" /\ input.kind = "opaque"
       THEN [result EXCEPT !.origins = {<<"runtime", "runtime", "consumer">>}] ELSE result
PromoteDependency(input) ==
    LET result == CorrectProjection(input)
    IN IF result.error = "" THEN [result EXCEPT !.direct_data = @ \cup {input.runtime_path}] ELSE result
RestrictOrdinaryAsset(input) ==
    LET result == CorrectProjection(input)
    IN IF input.member THEN [result EXCEPT !.copyable = {}] ELSE result
RebindOwner(input) ==
    LET result == CorrectProjection(input)
    IN IF result.error = "" THEN [result EXCEPT !.owners = {"consumer"}] ELSE result
DropForwardedRoot(input) ==
    [CorrectProjection(input) EXCEPT !.selected = {pair \in @: pair[1] # input.request}]
DropIdentityDiscovery(input) ==
    [CorrectProjection(input) EXCEPT !.discovery = {pair \in @: pair[1] # pair[2]}]
InventOpaqueRootOrigin(input) ==
    [CorrectProjection(input) EXCEPT !.runner_origins = @ \cup {<<input.runtime_id, input.runtime_id>>}]
DiscoverUnselectedAlias(input) ==
    [CorrectProjection(input) EXCEPT !.discovery = SelectedRoots([input EXCEPT !.roots = @ \cup input.extra_inputs])]

Init == /\ caseName \in IF Seed = "all" THEN Cases ELSE {Seed}
        /\ phase = "assembling" /\ observed = EmptyObservation
Publish == /\ phase = "assembling"
           /\ observed' = Projection(Input(caseName))
           /\ phase' = IF observed'.error = "" THEN "published" ELSE "rejected"
           /\ UNCHANGED caseName
vars == <<caseName, phase, observed>>
Next == Publish \/ (phase # "assembling" /\ UNCHANGED vars)
Spec == Init /\ [][Next]_vars

TypeOK == /\ caseName \in Cases /\ phase \in {"assembling", "published", "rejected"}
          /\ observed.live \subseteq {"source", "runtime"} /\ observed.owners \subseteq {"producer", "peer", "consumer"}
ValidPublicationAccepted == phase # "assembling" /\ Failure(Input(caseName)) = "" => phase = "published"
InvalidFactsReject == phase # "assembling" /\ Failure(Input(caseName)) # "" => phase = "rejected"
ProducerOriginSurvives == phase = "published" => observed.origins = RequiredOrigins(Input(caseName))
ProducerOwnerSurvives == phase = "published" => observed.owners = ProducerOwners(Input(caseName))
CanonicalFileRemainsLive == phase = "published" => observed.live = {Input(caseName).runtime_id}
ExplicitCoordinatesSurvive == phase = "published" => RequiredViews(Input(caseName)) \subseteq observed.views
DirectPublicationMembership == phase = "published" =>
    observed.direct_data = DirectData(Input(caseName)) /\ observed.direct_js = DirectJS(Input(caseName))
OrdinaryMemberAssetRemainsCopyable == phase = "published" /\ Input(caseName).member =>
    observed.copyable = DirectData(Input(caseName))
OpaqueFactsRemainOpaque == phase = "published" /\ Input(caseName).kind = "opaque" => observed.origins = {}
RequestedRootsRemainSelected == phase = "published" => observed.selected = SelectedRoots(Input(caseName))
DiscoveryUsesRequestedInputs == phase = "published" => observed.discovery = SelectedRoots(Input(caseName))
RunnerOriginsStayProducing == phase = "published" => observed.runner_origins = RunnerOrigins(Input(caseName))

=============================================================================
