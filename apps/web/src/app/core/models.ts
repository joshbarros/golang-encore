export interface Gig {
  id: string;
  code: string;
  name: string;
  stage: string;
  band: string;
  gigDate: string;
  priceCents: number;
  seatsTotal: number;
  seatsLeft: number;
  maxPerFan: number;
  nextSeatPriceCents: number | null;
  tier: string | null;
}

export type OrderStatus = 'Requested' | 'Held' | 'Sold' | 'Refunded' | 'Rejected';

export interface Order {
  id: string;
  gigCode: string;
  ownerId: string;
  reference: string;
  seats: number;
  totalCents: number;
  status: OrderStatus;
  reason: string | null;
  passNumber: string | null;
  createdAt: string;
  updatedAt: string;
  seatsReleasedAt: string | null;
}

export interface LogEntry {
  id: string;
  type: string;
  module: string;
  occurredAt: string;
}

export interface UserInfo {
  id: string;
  email: string;
  roles: string[];
}

export interface AuthResponse {
  accessToken: string;
  expiresAt: string;
  user: UserInfo;
}
