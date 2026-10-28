// k6 load test for POST /v1/me/transfers. Each VU sends 1-kobo transfers
// from its own seeded sender to that sender's recipient, with a fresh
// Idempotency-Key per request. Seed with internal/stresstest/k6seed.
//
//   k6 run -e BASE_URL=http://localhost:8080 -e PAIRS=/data/k6-pairs.json \
//     --vus 50 --duration 30s scripts/stress/transfers.js
import http from 'k6/http';
import { check } from 'k6';
import { SharedArray } from 'k6/data';

const pairs = new SharedArray('pairs', () => JSON.parse(open(__ENV.PAIRS)));
const base = __ENV.BASE_URL || 'http://localhost:8080';

export const options = {
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max'],
  discardResponseBodies: true,
};

export default function () {
  const p = pairs[(__VU - 1) % pairs.length];
  const res = http.post(
    `${base}/v1/me/transfers`,
    JSON.stringify({ recipient_email: p.to_email, recipient_name: p.to_name, amount_minor: 1, currency: 'NGN' }),
    {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${p.token}`,
        'Idempotency-Key': `k6-${__VU}-${__ITER}-${Date.now()}`,
      },
    },
  );
  check(res, { 'status is 201': (r) => r.status === 201 });
}
