select p.* from paymentevents p where p.OrderID = $1 and p.RegionID = $2;
