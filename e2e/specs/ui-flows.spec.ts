import { test, expect, Page } from '@playwright/test';
import { ADMIN, signIn } from './support';

const run = () => Date.now().toString(36).toUpperCase() + Math.floor(Math.random() * 1000);

async function openAccount(
  page: Page,
  code: string,
  capacity: number,
  shown: string,
  limit = capacity,
) {
  await signIn(page, ADMIN);
  await page.getByLabel('Gig code').fill(code);
  await page.getByLabel('Seats on sale').fill(String(capacity));
  await page.getByLabel('Limit per fan').fill(String(limit));
  await page.getByRole('button', { name: 'Add gig' }).click();
  await expect(page.getByRole('heading', { name: `Buy seats for ${code}` })).toBeVisible();
  await expect(page.getByTestId('selected-remaining')).toHaveText(shown);
}

async function submit(page: Page, reference: string, quantity: number) {
  await page.getByLabel('Fan reference').fill(reference);
  await page.getByLabel('Seats', { exact: true }).fill(String(quantity));
  await page.getByRole('button', { name: 'Buy' }).click();
}

const row = (page: Page, reference: string) => page.locator(`tr[data-reference="${reference}"]`);

test('the API refuses bad input and the page says why', async ({ page }) => {
  const id = run();
  await openAccount(page, `UI-ERR-${id}`, 5, '5');

  await submit(page, `FAN-${id}`, 7);
  await expect(page.getByRole('alert')).toContainText('at most 6 seats');

  await submit(page, ' ', 1);
  await expect(page.getByRole('alert')).toContainText(/reference is required/i);

  await submit(page, `FAN-${id}`, 1);
  await expect(row(page, `FAN-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await submit(page, `FAN-${id}`, 1);
  await expect(page.getByRole('alert')).toContainText('already');
  await expect(page.locator('tbody tr[data-reference]')).toHaveCount(1);
});

test('a poison reference is rejected, gets no document, and the amount comes back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-POI-${id}`, 5, '5');

  await submit(page, `FAN-POISON-${id}`, 2);
  const poisoned = row(page, `FAN-POISON-${id}`);
  await expect(poisoned.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(poisoned).toContainText('poison');
  await expect(poisoned.getByTestId('pass')).toHaveText('—');
  await expect(poisoned.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');
});

test('the last seat goes to one request and the same fan asking again is held to the limit', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LAST-${id}`, 1, '1');

  await submit(page, `FAN-A-${id}`, 1);
  await submit(page, `FAN-B-${id}`, 1);

  const statuses = page.locator('tbody tr[data-reference] [data-testid="status"]');
  await expect(statuses).toHaveCount(2);
  await expect
    .poll(async () => (await statuses.allTextContents()).map((s) => s.trim()).sort(), {
      timeout: 20_000,
    })
    .toEqual(['Sold', 'Rejected'].sort());
  await expect(page.locator('tbody')).toContainText('per fan');
  await expect(page.getByTestId('selected-remaining')).toHaveText('0');
});

test('the event log shows the Kafka steps and the reversal keeps the document', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-LOG-${id}`, 4, '4');

  await submit(page, `FAN-${id}`, 2);
  const mine = row(page, `FAN-${id}`);
  await expect(mine.getByTestId('status')).toHaveText('Sold', { timeout: 20_000 });

  await mine.getByRole('button', { name: 'Event log' }).click();
  const log = page.getByRole('region', { name: 'Kafka event log' });
  await expect(log).toContainText('seats.reserve', { timeout: 20_000 });
  await expect(log).toContainText('seats.held');
  await expect(log).toContainText('pass.issued');

  await mine.getByRole('button', { name: 'Refund' }).click();
  await expect(mine.getByTestId('status')).toHaveText('Refunded');
  await expect(mine.getByRole('button', { name: 'Refund' })).toHaveCount(0);
  await expect(mine.getByTestId('pass')).toHaveText(/EN-[0-9A-F]{12}/);
  await expect(page.getByTestId('selected-remaining')).toHaveText('4');
  await expect(log).toContainText('seats.released', { timeout: 20_000 });
});

test('one fan cannot go past the limit per fan, and a refund gives the allowance back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-CAP-${id}`, 10, '10', 2);
  await expect(page.getByTestId(`remaining-UI-CAP-${id}`)).toContainText('max 2 each');

  await submit(page, `FAN-ONE-${id}`, 2);
  await expect(row(page, `FAN-ONE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });

  await submit(page, `FAN-TWO-${id}`, 1);
  const capped = row(page, `FAN-TWO-${id}`);
  await expect(capped.getByTestId('status')).toHaveText('Rejected', { timeout: 20_000 });
  await expect(capped).toContainText('at most 2 seats per fan');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');

  await row(page, `FAN-ONE-${id}`).getByRole('button', { name: 'Refund' }).click();
  await expect(row(page, `FAN-ONE-${id}`).getByTestId('status')).toHaveText('Refunded', {
    timeout: 20_000,
  });
  await expect(page.getByTestId('selected-remaining')).toHaveText('10');

  await submit(page, `FAN-THREE-${id}`, 2);
  await expect(row(page, `FAN-THREE-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
});

test('seat prices step up as the gig sells, an order pays each seat its own price, and a refund steps back', async ({
  page,
}) => {
  const id = run();
  await openAccount(page, `UI-STEP-${id}`, 8, '8');
  // 8 seats at a list price of $50.00: 2 early-bird seats at $40.00, 4 standard, 2 last seats at $62.50.
  await expect(page.getByTestId('next-price')).toContainText('$40.00');
  await expect(page.getByTestId('next-price')).toContainText('early bird');

  await submit(page, `FAN-A-${id}`, 3); // 40 + 40 + 50 = 130.00, plus 12% = 145.60
  await expect(row(page, `FAN-A-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await expect(row(page, `FAN-A-${id}`)).toContainText('$145.60');
  await expect(page.getByTestId('next-price')).toContainText('$50.00');
  await expect(page.getByTestId('next-price')).toContainText('standard');

  await submit(page, `FAN-B-${id}`, 3); // places 3-5, 50 x 3 = 150.00, plus 12% = 168.00
  await expect(row(page, `FAN-B-${id}`).getByTestId('status')).toHaveText('Sold', {
    timeout: 20_000,
  });
  await expect(row(page, `FAN-B-${id}`)).toContainText('$168.00');
  await expect(page.getByTestId('next-price')).toContainText('$62.50');
  await expect(page.getByTestId('next-price')).toContainText('last seats');

  // FAN-B cancels: three seats come back, so the price steps back down from last seats to standard.
  await row(page, `FAN-B-${id}`).getByRole('button', { name: 'Refund' }).click();
  await expect(row(page, `FAN-B-${id}`).getByTestId('status')).toHaveText('Refunded', {
    timeout: 20_000,
  });
  await expect(page.getByTestId('next-price')).toContainText('$50.00');
  await expect(page.getByTestId('next-price')).toContainText('standard');
});
