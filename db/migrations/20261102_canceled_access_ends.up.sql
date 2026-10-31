-- A licence stops when its Stripe subscription ends. Until now a renewal
-- that failed moved the licence's expiry to the end of the period nobody
-- paid for, and the end of the subscription, when its payment retries ran
-- out, kept that date: the licence went on working as "canceled" until
-- then, up to a year for a yearly plan. A licence is canceled only when
-- its subscription ended, so one whose expiry lies after that moment
-- ends at that moment, as it now does when the subscription ends.
UPDATE licenses SET valid_until = COALESCE(canceled_at, now()), updated_at = now()
WHERE status = 'canceled' AND valid_until > COALESCE(canceled_at, now());
