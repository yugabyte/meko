/*+ IndexScan(eventchains_1 ux_auditchains_1) */
with auditChainData as (
  select
    $1 as NewChainID,
    $2 as RegionID,
    $3 as Identifier,
    $4 as Discriminator,
    $5 as TypeID,
    $6 as Version,
    $7::jsonb as CurrentState,
    $8::jsonb as NewState,
    $9 as Now,
    $10 as UserID
), inserted as (
  insert into eventchains_1 (ID, RegionID, Identifier, Discriminator, TypeID, Version, State, CreatedByID, CreationTime)
  select d.NewChainID, d.RegionID, d.Identifier, d.Discriminator, d.TypeID, d.Version, d.NewState, d.UserID, d.Now
  from auditChainData d
  where not exists (
    select 1
    from eventchains_1
    where Identifier = d.Identifier
      and Discriminator = d.Discriminator
      and Version = d.Version
      and TypeID = d.TypeID
      and IsOffline = false
      and OrganizationUnitID = empty_uuid()
  )
  on conflict (Identifier, Discriminator, TypeID, Version, OrganizationUnitID, IsOffline) do nothing
  returning *
), updated as (
  update eventchains_1 as ac
  set State = auditChainData.NewState
  from auditChainData
  where ac.Identifier = auditChainData.Identifier
    and ac.Discriminator = auditChainData.Discriminator
    and ac.Version = auditChainData.Version
    and ac.TypeID = auditChainData.TypeID
    and ac.State = auditChainData.CurrentState
  returning
    ac.ID,
    ac.RegionID,
    COALESCE((ac.State->>'seq')::bigint, 0) as CurrentSequenceNumber,
    COALESCE((ac.State->>'seq')::bigint, 0) as PreviousSequenceNumber,
    COALESCE(ac.State->>'sig', '') as CurrentSignature,
    COALESCE(ac.State->>'sig', '') as PreviousSignature
), insertLedgerForNewChain as (
  insert into eventchainledger_1 (ID, RegionID, ChainID, NewSequenceNumber, NewSignature, CreatedByID, CreationTime)
  select $11, RegionID, ID, COALESCE((State->>'seq')::bigint, 0), COALESCE(State->>'sig', ''), CreatedByID, CreationTime
  from inserted
  returning ChainID as ID
), insertLedgerForExistingChain as (
  insert into eventchainledger_1 (ID, RegionID, ChainID, NewSequenceNumber, PreviousSequenceNumber, NewSignature, PreviousSignature, CreatedByID, CreationTime)
  select $11, u.RegionID, u.ID, u.CurrentSequenceNumber, u.PreviousSequenceNumber,
         u.CurrentSignature, u.PreviousSignature, $10, $9
  from updated as u
  returning ChainID as ID
)
select ID from insertLedgerForNewChain
union all
select ID from insertLedgerForExistingChain;
