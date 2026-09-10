select ip.*, pt.*
from billingpayments ip
left join paymentevents pt on (ip.PaymentTransactionID = pt.ID) and (pt.RegionID = $1)
where ip.InvoiceID = $2 and ip.RegionID = $1;
