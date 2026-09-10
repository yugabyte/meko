select o.*, shipping.*, billing.*, pickup.*, u.*, uc.*
from sales o
left join locations shipping on o.ShippingAddressID = shipping.ID and shipping.RegionID = $1
left join locations billing on o.BillingAddressID = billing.ID and billing.RegionID = $1
left join locations pickup on o.PickupAddressID = pickup.ID and pickup.RegionID = $1
left join accounts u on o.CustomerID = u.ID and u.RegionID = $1
left join businesses uc on u.CompanyID = uc.ID and uc.RegionID = $1
where o.ID = $2 and o.RegionID = $1;
