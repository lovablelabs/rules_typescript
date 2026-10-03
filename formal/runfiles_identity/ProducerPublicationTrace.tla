---------------------- MODULE ProducerPublicationTrace ----------------------
EXTENDS ProducerPublication

TracePending == phase = "assembling"
ReplayState == [witness_count |-> Cardinality(Cases), phase |-> phase, input |-> Input(caseName), expected |-> observed]

=============================================================================
