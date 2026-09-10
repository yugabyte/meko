select i.*, imd.*, iud.*, iudc.*, iudouc.*, iudouca.*, iudba.*, iudsa.*, iudoua.*
from billings i
left join billinguserdata iud on (i.UserDataID = iud.id) and (iud.RegionID = $1)
left join businesses iudc on (iud.CompanyID = iudc.ID) and (iudc.RegionID = $1)
left join businesses iudouc on (iud.OrganizationUnitCompanyID = iudouc.ID) and (iudouc.RegionID = $1)
left join locations iudouca on (iudouc.AddressID = iudouca.ID) and (iudouca.RegionID = $1)
left join locations iudba on (iud.BillingAddressID = iudba.ID) and (iudba.RegionID = $1)
left join locations iudsa on (iud.ShippingAddressID = iudsa.ID) and (iudsa.RegionID = $1)
left join locations iudoua on (iud.OrganizationUnitAddressID = iudoua.ID) and (iudoua.RegionID = $1)
left join billingmanualdata imd on (i.ID = imd.InvoiceID) and (imd.RegionID = $1)
where i.ID = $2 and i.RegionID = $1;
