with insert_into_product_ledger as (
  insert into itemledger_1 (ID, RegionID, TypeID, ProductID, OrganizationUnitID, Quantity, QuantityAllocated, Remark, StockLabelID, IsSummary, StockMutationID, OrderLineID, CreatedByID, CreationTime)
  select ID, $1, TypeID, ProductID, OrganizationUnitID, Quantity, QuantityAllocated, Remark, StockLabelID, IsSummary, StockMutationID, OrderLineID, CreatedByID, CreationTime
  from unnest($2)
  returning ProductID, OrganizationUnitID, StockLabelID, Quantity, QuantityAllocated
), inserted_product_ledger AS (
  select d.ProductID, d.OrganizationUnitID, d.StockLabelID, sum(Quantity) as Quantity, sum(QuantityAllocated) as QuantityAllocated
  from insert_into_product_ledger d
  group by d.OrganizationUnitID, d.ProductID, d.StockLabelID
), updated AS (
  update inventory_1
  set QuantityOnHand = inserted_product_ledger.Quantity + inventory_1.QuantityOnHand,
      QuantityAllocated = inserted_product_ledger.QuantityAllocated + inventory_1.QuantityAllocated
  from inserted_product_ledger
  where inventory_1.ProductID = inserted_product_ledger.ProductID
    AND inventory_1.OrganizationUnitID = inserted_product_ledger.OrganizationUnitID
    AND inventory_1.RegionID = $1
    AND inventory_1.StockLabelID = inserted_product_ledger.StockLabelID
  returning inventory_1.ProductID, inventory_1.OrganizationUnitID, inventory_1.StockLabelID
), inserted AS (
  insert into inventory_1 (Productid, OrganizationunitID, RegionID, QuantityOnHand, QuantityAllocated, StockLabelID)
  select inserted_product_ledger.ProductID, inserted_product_ledger.OrganizationUnitID, $1,
         inserted_product_ledger.Quantity, inserted_product_ledger.QuantityAllocated,
         inserted_product_ledger.StockLabelID
  from inserted_product_ledger
  left join updated on inserted_product_ledger.ProductID = updated.ProductID
                  and inserted_product_ledger.OrganizationUnitID = updated.OrganizationUnitID
                  and inserted_product_ledger.StockLabelID = updated.StockLabelID
  where updated.ProductID is null
  on conflict (OrganizationUnitID, ProductID, StockLabelID, RegionID)
  do update
     set QuantityOnHand = inventory_1.QuantityOnHand + EXCLUDED.QuantityOnHand,
         QuantityAllocated = inventory_1.QuantityAllocated + EXCLUDED.QuantityAllocated
  returning inventory_1.ProductID
)
select $3 from updated
union
select $4 from inserted;
