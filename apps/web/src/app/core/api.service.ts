import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Order, LogEntry, Gig } from './models';

export interface NewGig {
  code: string;
  name: string;
  stage: string;
  band: string;
  seatsTotal: number;
  maxPerFan: number;
  priceCents: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  gigs = () => firstValueFrom(this.http.get<Gig[]>('/api/gigs'));
  openGig = (body: NewGig) => firstValueFrom(this.http.post<Gig>('/api/gigs', body));

  orders = (gigCode: string) =>
    firstValueFrom(this.http.get<Order[]>('/api/orders', { params: { gigCode } }));
  book = (body: { gigCode: string; reference: string; seats: number }) =>
    firstValueFrom(this.http.post<Order>('/api/orders', body));
  refund = (id: string) => firstValueFrom(this.http.post<Order>(`/api/orders/${id}/refund`, {}));
  events = (id: string) => firstValueFrom(this.http.get<LogEntry[]>(`/api/orders/${id}/events`));
}
