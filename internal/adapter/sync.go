package adapter

func SyncPaymentStatusFromPaymentStatus(status PaymentStatus) SyncPaymentStatus {
	switch status {
	case PaymentStatusSuccess:
		return SyncPaymentStatusPaid
	case PaymentStatusFailed:
		return SyncPaymentStatusFailed
	case PaymentStatusCanceled, PaymentStatusExpired:
		return SyncPaymentStatusFailed
	default:
		return SyncPaymentStatusPending
	}
}

func SyncRefundStatusFromPaymentStatus(status PaymentStatus) SyncRefundStatus {
	switch status {
	case PaymentStatusRefunded:
		return SyncRefundStatusRefunded
	default:
		return SyncRefundStatusNone
	}
}
