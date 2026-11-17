# Decision records

An accepted record is not rewritten. It states what was decided and why at the time. The list of records, with their
status and the architecture they shape, is under
[Source decisions](../architecture/README.md#source-decisions) in the architecture overview. Two things have changed
since the earlier records were written, and this note applies from 2026-11-17.

## "The control plane" is dac-plane

Records written before 2026-11-17 call dac-plane "the control plane". It is the same system. The name changed because
dac-plane is the platform a company builds an agent product on, not a layer that controls someone else's compute.
Read "the control plane" in a record as dac-plane. Titles and file names keep their original wording so that links
stay stable.

## There is no first-party web workspace

[ADR-0007](0007-surface-api-transport-and-authentication.md) and the
[2026-10-15 review](../architecture/review-2026-10-15.md) mention a web workspace that would come later. dac-plane does
not ship one, or any other surface. The company that deploys dac-plane builds the surfaces its users work in, against
the Surface API. A reference application in a separate repository
([#97](https://github.com/dac-plane/dac-plane/issues/97)) shows how a surface uses the API; it is an example and not a
product. What ADR-0007 decides about the Surface API is unchanged.
