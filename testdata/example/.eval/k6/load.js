import http from 'k6/http';
import { check } from 'k6';

const fixtures = JSON.parse(open(__ENV.TOKENS_FILE));

export default function () {
  const user = fixtures[Math.floor(Math.random() * fixtures.length)];
  const res = http.get(`${__ENV.API_URL}/items`, { headers: { Authorization: `Bearer ${user.token}` } });
  check(res, { ok: (r) => r.status === 200 });
}
